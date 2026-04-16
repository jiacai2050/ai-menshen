package aimenshen

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jiacai2050/ai-menshen/internal/web"
)

const (
	authHeaderName      = "Authorization"
	reportModelsPath    = "/__report/models"
	reportSummaryPath   = "/__report/summary"
	reportDailyPath     = "/__report/daily"
	reportLogsPath      = "/__report/logs"
	reportLogDetailPath = "/__report/log"
)

var streamBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, 16*1024)
		return &buffer
	},
}

type Gateway struct {
	cfg               Config
	activeProviders   []ProviderConfig
	activeTotalWeight int
	storage           *Storage
	clients           map[string]*http.Client // keyed by provider proxy URL ("" = use env proxy settings)
}

func NewGateway(cfg Config, storage *Storage) (*Gateway, error) {
	activeProviders, activeTotalWeight := buildActiveProviders(cfg.Providers)
	if len(activeProviders) == 0 || activeTotalWeight <= 0 {
		return nil, fmt.Errorf("gateway requires at least one active provider")
	}

	timeout := time.Duration(cfg.Upstream.Timeout) * time.Second
	clients := make(map[string]*http.Client)
	for _, p := range activeProviders {
		if _, exists := clients[p.Proxy]; exists {
			continue
		}
		transport, err := buildTransport(p.Proxy, cfg.Upstream.MaxIdleConnsPerHost)
		if err != nil {
			return nil, fmt.Errorf("build transport for proxy %q: %w", p.Proxy, err)
		}
		clients[p.Proxy] = &http.Client{Transport: transport, Timeout: timeout}
	}

	service := &Gateway{
		cfg:               cfg,
		activeProviders:   activeProviders,
		activeTotalWeight: activeTotalWeight,
		storage:           storage,
		clients:           clients,
	}

	return service, nil
}

func buildActiveProviders(providers []ProviderConfig) ([]ProviderConfig, int) {
	activeProviders := make([]ProviderConfig, 0, len(providers))
	totalWeight := 0
	for _, provider := range providers {
		if provider.Weight <= 0 {
			continue
		}
		activeProviders = append(activeProviders, provider)
		totalWeight += provider.Weight
	}

	return activeProviders, totalWeight
}

func buildTransport(proxyURL string, maxIdleConnsPerHost int) (*http.Transport, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL %q: %w", proxyURL, err)
		}
		t.Proxy = http.ProxyURL(parsed)
	}
	return t, nil
}

func (g *Gateway) clientForProvider(provider ProviderConfig) *http.Client {
	return g.clients[provider.Proxy]
}

// pickProvider selects from the startup-precomputed active provider set by
// drawing one random number in [0, totalWeight) and walking the weights until
// the cumulative range covers that value.
func (g *Gateway) pickProvider() ProviderConfig {
	if len(g.activeProviders) == 1 {
		return g.activeProviders[0]
	}

	pick := mrand.IntN(g.activeTotalWeight)
	for _, provider := range g.activeProviders {
		pick -= provider.Weight
		if pick < 0 {
			return provider
		}
	}

	return g.activeProviders[len(g.activeProviders)-1]
}

// shouldFailover returns true if the upstream response indicates we should
// try the next provider: network errors, HTTP 5xx, or 429 (rate limited).
func shouldFailover(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	return resp != nil && (resp.StatusCode >= 500 || resp.StatusCode == 429)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Silent rejection for common browser noise
	if r.URL.Path == "/favicon.ico" || r.URL.Path == "/robots.txt" || strings.HasPrefix(r.URL.Path, "/apple-touch-icon") {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// 2. Optional auth check for all routes (Proxy, Dashboard, Reports, Assets)
	if g.cfg.Auth.Enable {
		isUI := r.URL.Path == "/" ||
			strings.HasPrefix(r.URL.Path, "/__report/") ||
			strings.HasPrefix(r.URL.Path, "/assets/")

		if isUI {
			// Browser-friendly Basic Auth for UI and Assets
			user, pass, ok := r.BasicAuth()
			if !ok || user != g.cfg.Auth.User || pass != g.cfg.Auth.Password {
				w.Header().Set("WWW-Authenticate", `Basic realm="ai-menshen"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		} else {
			// OpenAI-compatible Bearer Token for API
			authHeader := r.Header.Get(authHeaderName)
			expected := "Bearer " + g.cfg.Auth.Token
			if authHeader != expected {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}

	// 3. UI, Report APIs, and Static Assets
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		g.handleDashboard(w)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/assets/") {
		g.handleAssets(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/__report/") {
		switch r.URL.Path {
		case reportModelsPath:
			g.handleModelReport(w, r)
			return
		case reportSummaryPath:
			g.handleSummaryReport(w, r)
			return
		case reportDailyPath:
			g.handleDailyReport(w, r)
			return
		case reportLogsPath:
			g.handleLogsReport(w, r)
			return
		case reportLogDetailPath:
			g.handleLogDetailReport(w, r)
			return
		}
	}

	// 4. Proxy Path
	if !isAuditablePath(r.URL.Path) {
		g.proxyPassthrough(w, r)
		return
	}

	startedAt := time.Now()

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	provider := g.pickProvider()

	meta, err := AnalyzeRequest(r.URL.Path, requestBody, provider, g.cfg.Cache.Enable)
	if err != nil {
		logError("failed to analyze request: %v", err)
		// Fallback to original body and default meta
		meta = RequestMeta{
			EffectiveBody: requestBody,
		}
	}

	if g.cfg.Verbose && len(meta.EffectiveBody) > 0 {
		logInfo("Request Body: %s", meta.EffectiveBody)
	}

	requestLog := RequestLog{
		ID:          newRequestID(),
		CreatedAt:   startedAt,
		Path:        r.URL.Path,
		Model:       meta.EffectiveModel,
		CacheKey:    meta.CacheKey,
		RequestBody: g.requestBodyForStorage(meta),
	}

	if canUseCache(r, meta, g.cfg.Cache) {
		cached, err := g.storage.FindCachedResponse(meta.CacheKey, g.cfg.Cache.MaxAge)
		if err != nil {
			logError("cache lookup failed: %v", err)
		}
		if cached != nil {
			g.serveCachedResponse(w, r, startedAt, requestLog, cached, meta.Stream)
			return
		}
	}

	if meta.Stream {
		g.proxyStream(w, r, requestBody, meta, requestLog, provider)
		return
	}

	// Non-stream: try provider, failover to remaining active providers on failure
	resp, duration, err := g.forwardUpstream(r, bytes.NewReader(meta.EffectiveBody), provider)
	if shouldFailover(resp, err) && g.cfg.Failover.Enable && len(g.activeProviders) > 1 {
		if err != nil {
			logError("[%s] upstream request failed for %s (%.3fs): %v", requestLog.ID, provider.BaseURL, duration.Seconds(), err)
		} else {
			drainAndCloseBody(resp)
			logError("[%s] upstream returned %d from %s (%.3fs)", requestLog.ID, resp.StatusCode, provider.BaseURL, duration.Seconds())
		}
		resp, duration, meta = g.failoverNonStream(r, requestBody, &requestLog, provider)
	}

	if err != nil && resp == nil {
		g.saveExchange(requestLog, ResponseLog{
			RequestID:  requestLog.ID,
			StatusCode: http.StatusBadGateway,
			DurationMS: duration.Milliseconds(),
		}, nil)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logError("[%s] failed to read upstream response: %v", requestLog.ID, err)
		g.saveExchange(requestLog, ResponseLog{
			RequestID:  requestLog.ID,
			StatusCode: http.StatusBadGateway,
			DurationMS: duration.Milliseconds(),
		}, nil)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	if g.cfg.Verbose && len(responseBody) > 0 {
		logInfo("Response Body: %s", responseBody)
	}

	usage, err := ExtractUsage(requestLog.ID, responseBody)
	if err != nil {
		logError("[%s] usage extraction failed: %v", requestLog.ID, err)
	}

	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   resp.StatusCode,
		ResponseBody: g.responseBodyForStorage(r, meta, resp.StatusCode, responseBody),
		DurationMS:   duration.Milliseconds(),
	}
	g.saveExchange(requestLog, responseLog, usage)

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(responseBody); err != nil {
		logError("[%s] write response failed: %v", requestLog.ID, err)
	}

	logInfo("[%s] [%d] %s %s (%.3fs)", requestLog.ID, resp.StatusCode, r.Method, r.URL.String(), duration.Seconds())
}

func (g *Gateway) proxyStream(w http.ResponseWriter, r *http.Request, requestBody []byte, meta RequestMeta, requestLog RequestLog, provider ProviderConfig) {
	resp, duration, err := g.forwardUpstream(r, bytes.NewReader(meta.EffectiveBody), provider)
	if shouldFailover(resp, err) && g.cfg.Failover.Enable && len(g.activeProviders) > 1 {
		if err != nil {
			logError("[%s] stream upstream request failed for %s (%.3fs): %v", requestLog.ID, provider.BaseURL, duration.Seconds(), err)
		} else {
			drainAndCloseBody(resp)
			logError("[%s] stream upstream returned %d from %s (%.3fs)", requestLog.ID, resp.StatusCode, provider.BaseURL, duration.Seconds())
		}
		resp, duration, meta = g.failoverNonStream(r, requestBody, &requestLog, provider)
	}

	if err != nil && resp == nil {
		g.saveExchange(requestLog, ResponseLog{
			RequestID:  requestLog.ID,
			StatusCode: http.StatusBadGateway,
			DurationMS: duration.Milliseconds(),
		}, nil)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	usageExtractor := NewSSEUsageExtractor(requestLog.ID)
	var captured *bytes.Buffer
	var limit int64
	if g.cfg.Logging.LogResponseBody || g.cfg.Verbose || canUseCache(r, meta, g.cfg.Cache) {
		captured = &bytes.Buffer{}
		if g.cfg.Logging.LogResponseBody || g.cfg.Verbose {
			limit = 0 // No limit for logging/verbose
		} else {
			limit = g.cfg.Cache.MaxBodyBytes
		}
	}
	bufferPtr := streamBufferPool.Get().(*[]byte)
	buffer := *bufferPtr
	defer streamBufferPool.Put(bufferPtr)

	var readFailed bool
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if _, err := w.Write(chunk); err != nil {
				logError("[%s] stream response write failed: %v", requestLog.ID, err)
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			if err := usageExtractor.Write(chunk); err != nil {
				logError("[%s] stream usage extraction failed: %v", requestLog.ID, err)
				// Do not break, keep proxying the stream
			}
			if captured != nil {
				if limit == 0 || int64(captured.Len()) < limit {
					_, _ = captured.Write(chunk)
				}
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			logError("[%s] stream response copy failed: %v", requestLog.ID, readErr)
			readFailed = true
			break
		}
	}

	elapsed := duration
	if total := time.Since(requestLog.CreatedAt); total > elapsed {
		elapsed = total
	}

	if err := usageExtractor.Finalize(); err != nil {
		logError("[%s] stream usage extraction failed: %v", requestLog.ID, err)
	}

	if captured != nil && captured.Len() > 0 {
		if g.cfg.Verbose {
			logInfo("Stream Response Body: %s", captured.Bytes())
		}
	}

	var capturedBytes []byte
	if captured != nil {
		capturedBytes = captured.Bytes()
	}

	statusCode := resp.StatusCode
	if readFailed {
		statusCode = http.StatusBadGateway
	}

	responseBody := g.responseBodyForStorage(r, meta, statusCode, capturedBytes)

	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   statusCode,
		ResponseBody: responseBody,
		DurationMS:   elapsed.Milliseconds(),
	}
	g.saveExchange(requestLog, responseLog, usageExtractor.Usage())

	logInfo("[%s] [%d] %s %s (%.3fs)", requestLog.ID, statusCode, r.Method, r.URL.String(), elapsed.Seconds())
}

func (g *Gateway) serveCachedResponse(w http.ResponseWriter, r *http.Request, startedAt time.Time, requestLog RequestLog, cached *CachedResponse, isStream bool) {
	duration := time.Since(startedAt)

	responseLog := ResponseLog{
		RequestID:         requestLog.ID,
		StatusCode:        cached.StatusCode,
		ResponseBody:      g.cachedResponseBodyForStorage(cached.ResponseBody),
		DurationMS:        duration.Milliseconds(),
		FromCache:         true,
		CacheHitRequestID: cached.RequestID,
	}
	g.saveExchange(requestLog, responseLog, nil)

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(cached.StatusCode)

	if _, err := w.Write(cached.ResponseBody); err != nil {
		logError("[%s] write cached response failed: %v", requestLog.ID, err)
	}

	if isStream {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	logMsg := "cache hit"
	if isStream {
		logMsg = "stream cache hit"
	}
	logInfo("[%s] [%d] %s %s (%.3fs, %s)", requestLog.ID, cached.StatusCode, r.Method, r.URL.String(), duration.Seconds(), logMsg)
}

func (g *Gateway) handleModelReport(w http.ResponseWriter, r *http.Request) {
	days := g.getDays(r)
	reports, err := g.storage.ModelUsageReports(days)
	if err != nil {
		logError("model report error: %v", err)
		http.Error(w, "failed to query model report", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reports)
}

func (g *Gateway) handleSummaryReport(w http.ResponseWriter, r *http.Request) {
	days := g.getDays(r)
	summary, err := g.storage.UsageSummary(days)
	if err != nil {
		logError("summary report error: %v", err)
		http.Error(w, "failed to query summary report", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
}

func (g *Gateway) handleDailyReport(w http.ResponseWriter, r *http.Request) {
	days := g.getDays(r)
	daily, err := g.storage.DailyUsage(days)
	if err != nil {
		logError("daily report error: %v", err)
		http.Error(w, "failed to query daily report", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(daily)
}

func (g *Gateway) handleLogsReport(w http.ResponseWriter, r *http.Request) {
	days := g.getDays(r)
	logs, err := g.storage.RequestLogs(days, 1000)
	if err != nil {
		logError("logs report error: %v", err)
		http.Error(w, "failed to query logs report", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logs)
}

func (g *Gateway) handleLogDetailReport(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing request id", http.StatusBadRequest)
		return
	}

	detail, err := g.storage.RequestDetail(id)
	if err != nil {
		logError("log detail error: %v", err)
		http.Error(w, "failed to query log detail", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(detail)
}

func (g *Gateway) getDays(r *http.Request) int {
	daysStr := r.URL.Query().Get("days")
	days := 14 // default
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}
	return days
}

func (g *Gateway) handleDashboard(w http.ResponseWriter) {
	content, err := web.Assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "dashboard template not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write(content)
}

func (g *Gateway) handleAssets(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	content, err := web.Assets.ReadFile(path)
	if err != nil {
		http.Error(w, "asset not found", http.StatusNotFound)
		return
	}

	switch {
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript")
	}
	_, _ = w.Write(content)
}

func (g *Gateway) proxyPassthrough(w http.ResponseWriter, r *http.Request) {
	provider := g.pickProvider()
	resp, duration, err := g.forwardUpstream(r, r.Body, provider)
	if err != nil {
		logError("passthrough upstream request failed: %v", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logError("passthrough response copy failed: %v", err)
	}

	if g.cfg.Verbose {
		logInfo("[passthrough] [%d] %s %s (%.3fs)", resp.StatusCode, r.Method, r.URL.String(), duration.Seconds())
	}
}

func drainAndCloseBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (g *Gateway) failoverNonStream(r *http.Request, requestBody []byte, requestLog *RequestLog, tried ProviderConfig) (*http.Response, time.Duration, RequestMeta) {
	var lastResp *http.Response
	var lastDuration time.Duration
	var lastMeta RequestMeta
	for i, p := range g.activeProviders {
		if p.BaseURL == tried.BaseURL && p.APIKey == tried.APIKey {
			continue
		}

		meta, err := AnalyzeRequest(r.URL.Path, requestBody, p, g.cfg.Cache.Enable)
		if err != nil {
			logError("failed to analyze request for failover provider: %v", err)
			meta = RequestMeta{EffectiveBody: requestBody}
		}
		lastMeta = meta
		requestLog.Model = meta.EffectiveModel
		requestLog.CacheKey = meta.CacheKey
		requestLog.RequestBody = g.requestBodyForStorage(meta)
		logInfo("[%s] failover to provider %s (attempt %d/%d)", requestLog.ID, p.BaseURL, i+1, len(g.activeProviders))

		resp, duration, err := g.forwardUpstream(r, bytes.NewReader(meta.EffectiveBody), p)
		lastDuration = duration
		if err != nil {
			logError("[%s] upstream request failed for %s (%.3fs): %v", requestLog.ID, p.BaseURL, duration.Seconds(), err)
			continue
		}
		if !shouldFailover(resp, nil) {
			// Success
			drainAndCloseBody(lastResp)
			return resp, duration, meta
		}
		logError("[%s] upstream returned %d from %s (%.3fs)", requestLog.ID, resp.StatusCode, p.BaseURL, duration.Seconds())
		// Close previous failed resp, keep this one in case it's the last
		drainAndCloseBody(lastResp)
		lastResp = resp
	}
	// Return last resp (may be 5xx) so caller can pass it through
	return lastResp, lastDuration, lastMeta
}

func (g *Gateway) forwardUpstream(r *http.Request, body io.Reader, provider ProviderConfig) (*http.Response, time.Duration, error) {
	targetURL, err := buildUpstreamURL(provider.BaseURL, r.URL)
	if err != nil {
		return nil, 0, err
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("create upstream request: %w", err)
	}

	g.copyRequestHeaders(upstreamRequest.Header, r.Header, provider)
	g.applyProviderHeaders(upstreamRequest.Header, provider)

	startedAt := time.Now()
	resp, err := g.clientForProvider(provider).Do(upstreamRequest)
	duration := time.Since(startedAt)
	if err != nil {
		return nil, duration, fmt.Errorf("send upstream request: %w", err)
	}

	return resp, duration, nil
}

func (g *Gateway) applyProviderHeaders(headers http.Header, provider ProviderConfig) {
	// 1. Always clear the client's standard Authorization
	headers.Del(authHeaderName)

	// 2. Apply APIKey if present
	if provider.APIKey != "" {
		headers.Set(authHeaderName, "Bearer "+provider.APIKey)
	}

	// 3. Apply custom headers (can override Authorization if specified)
	for k, v := range provider.Headers {
		headers.Set(k, v)
	}
}

func buildUpstreamURL(baseURL string, incoming *url.URL) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse provider base URL: %w", err)
	}

	target := *base
	target.Path = joinURLPath(base.Path, incoming.Path)
	target.RawQuery = incoming.RawQuery
	return target.String(), nil
}

func joinURLPath(basePath, requestPath string) string {
	switch {
	case basePath == "":
		return requestPath
	case requestPath == "":
		return basePath
	case strings.HasSuffix(basePath, "/") && strings.HasPrefix(requestPath, "/"):
		return basePath + requestPath[1:]
	case strings.HasSuffix(basePath, "/") || strings.HasPrefix(requestPath, "/"):
		return basePath + requestPath
	default:
		return basePath + "/" + requestPath
	}
}

func (g *Gateway) copyRequestHeaders(dst, src http.Header, provider ProviderConfig) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		if g.isProviderHeader(key, provider) {
			continue
		}
		if strings.EqualFold(key, "Accept-Encoding") {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (g *Gateway) isProviderHeader(key string, provider ProviderConfig) bool {
	if strings.EqualFold(key, authHeaderName) {
		return true
	}
	for k := range provider.Headers {
		if strings.EqualFold(key, k) {
			return true
		}
	}
	return false
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (g *Gateway) requestBodyForStorage(meta RequestMeta) []byte {
	if !g.cfg.Logging.LogRequestBody {
		return nil
	}
	return meta.EffectiveBody
}

func (g *Gateway) responseBodyForStorage(r *http.Request, meta RequestMeta, statusCode int, responseBody []byte) []byte {
	if g.cfg.Logging.LogResponseBody || canStoreCachedResponse(r, meta, statusCode, responseBody, g.cfg.Cache) {
		return responseBody
	}
	return nil
}

func (g *Gateway) cachedResponseBodyForStorage(responseBody []byte) []byte {
	if g.cfg.Logging.LogResponseBody {
		return responseBody
	}
	return nil
}

func (g *Gateway) saveExchange(requestLog RequestLog, responseLog ResponseLog, usage *UsageLog) {
	if err := g.storage.SaveExchange(requestLog, responseLog, usage); err != nil {
		logError("save exchange failed: %v", err)
	}
}

func newRequestID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(bytes[:])
}
