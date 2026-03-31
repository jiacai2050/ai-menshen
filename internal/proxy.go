package aimenshen

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

type Gateway struct {
	cfg       Config
	providers []ProviderConfig
	storage   *Storage
	client    *http.Client
}

func NewGateway(cfg Config, storage *Storage) (*Gateway, error) {
	timeout := time.Duration(cfg.Upstream.Timeout) * time.Second
	service := &Gateway{
		cfg:       cfg,
		providers: cfg.Providers,
		storage:   storage,
		client:    &http.Client{Transport: http.DefaultTransport, Timeout: timeout},
	}

	return service, nil
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

	meta := ParseRequest(requestBody)

	if g.cfg.Verbose && len(meta.OriginalBody) > 0 {
		logInfo("Request Body: %s", string(meta.OriginalBody))
	}

	requestLog := RequestLog{
		ID:        newRequestID(),
		CreatedAt: startedAt,
		Path:      r.URL.Path,
	}

	fwd := g.forwardWithFailover(r, meta, &requestLog)

	if fwd.Cached != nil {
		g.serveCachedResponse(w, r, startedAt, requestLog, fwd.Cached, meta.Stream)
		return
	}

	if fwd.Err != nil {
		logError("[%s] upstream request failed (%.3fs): %v", requestLog.ID, fwd.Duration.Seconds(), fwd.Err)
		g.saveExchange(requestLog, ResponseLog{
			RequestID:  requestLog.ID,
			StatusCode: http.StatusBadGateway,
			DurationMS: fwd.Duration.Milliseconds(),
		}, nil)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	if meta.Stream {
		g.proxyStream(w, r, meta, requestLog, fwd)
		return
	}
	defer fwd.Resp.Body.Close()

	responseBody, err := io.ReadAll(fwd.Resp.Body)
	if err != nil {
		logError("[%s] failed to read upstream response: %v", requestLog.ID, err)
		g.saveExchange(requestLog, ResponseLog{
			RequestID:  requestLog.ID,
			StatusCode: http.StatusBadGateway,
			DurationMS: fwd.Duration.Milliseconds(),
		}, nil)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	if g.cfg.Verbose && len(responseBody) > 0 {
		logInfo("Response Body: %s", string(responseBody))
	}

	usage, err := ExtractUsage(requestLog.ID, responseBody)
	if err != nil {
		logError("[%s] usage extraction failed: %v", requestLog.ID, err)
	}

	storedResponseBody := responseBody
	if !g.cfg.Logging.LogResponseBody && !canStoreCachedResponse(r, requestLog.CacheKey, meta, fwd.Resp.StatusCode, responseBody, g.cfg.Cache) {
		storedResponseBody = nil
	}

	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   fwd.Resp.StatusCode,
		ResponseBody: storedResponseBody,
		DurationMS:   fwd.Duration.Milliseconds(),
	}
	g.saveExchange(requestLog, responseLog, usage)

	copyResponseHeaders(w.Header(), fwd.Resp.Header)
	w.WriteHeader(fwd.Resp.StatusCode)
	if _, err := w.Write(responseBody); err != nil {
		logError("[%s] write response failed: %v", requestLog.ID, err)
	}

	logInfo("[%s] [%d] %s %s (%.3fs)", requestLog.ID, fwd.Resp.StatusCode, r.Method, r.URL.String(), fwd.Duration.Seconds())
}

func (g *Gateway) proxyStream(w http.ResponseWriter, r *http.Request, meta RequestMeta, requestLog RequestLog, fwd forwardResult) {
	defer fwd.Resp.Body.Close()

	copyResponseHeaders(w.Header(), fwd.Resp.Header)
	w.WriteHeader(fwd.Resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	usageExtractor := NewSSEUsageExtractor(requestLog.ID)
	var captured *bytes.Buffer
	var limit int64
	shouldCapture := g.cfg.Logging.LogResponseBody || g.cfg.Verbose || (g.cfg.Cache.Enable && requestLog.CacheKey != "")
	if shouldCapture {
		captured = &bytes.Buffer{}
		if g.cfg.Logging.LogResponseBody || g.cfg.Verbose {
			limit = 0 // No limit for logging/verbose
		} else {
			limit = g.cfg.Cache.MaxBodyBytes
		}
	}
	// Use a fixed sized buffer for streaming to keep memory overhead predictable
	buffer := make([]byte, 16*1024)

	var readFailed bool
	for {
		n, readErr := fwd.Resp.Body.Read(buffer)
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

	elapsed := fwd.Duration
	if total := time.Since(requestLog.CreatedAt); total > elapsed {
		elapsed = total
	}

	if err := usageExtractor.Finalize(); err != nil {
		logError("[%s] stream usage extraction failed: %v", requestLog.ID, err)
	}

	if captured != nil && captured.Len() > 0 {
		if g.cfg.Verbose {
			logInfo("Stream Response Body: %s", captured.String())
		}
	}

	capturedBytes := []byte{}
	if captured != nil {
		capturedBytes = captured.Bytes()
	}

	statusCode := fwd.Resp.StatusCode
	if readFailed {
		statusCode = http.StatusBadGateway
	}

	storedResponseBody := capturedBytes
	if !g.cfg.Logging.LogResponseBody && !canStoreCachedResponse(r, requestLog.CacheKey, meta, statusCode, capturedBytes, g.cfg.Cache) {
		storedResponseBody = nil
	}

	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   statusCode,
		ResponseBody: storedResponseBody,
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
	resp, duration, err := g.forwardUpstream(r, r.Body, g.providers[0])
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

type forwardResult struct {
	Resp     *http.Response
	Cached   *CachedResponse
	Err      error
	Duration time.Duration
}

// forwardWithFailover prepares the request for each provider, checks cache (primary only),
// and tries upstream in order. It fills requestLog with the final provider's info.
func (g *Gateway) forwardWithFailover(r *http.Request, meta RequestMeta, requestLog *RequestLog) forwardResult {
	providers := g.providers
	if !g.cfg.FailoverEnabled() {
		providers = providers[:1]
	}

	var lastErr error
	var totalDuration time.Duration

	for i, provider := range providers {
		body, cacheKey, model := PrepareForProvider(r.URL.Path, meta, provider)
		requestLog.Model = model
		requestLog.CacheKey = cacheKey
		if g.cfg.Logging.LogRequestBody {
			requestLog.RequestBody = body
		}

		// Cache lookup for primary provider only
		if i == 0 && g.cfg.Cache.Enable && cacheKey != "" && r.Method == http.MethodPost {
			cached, err := g.storage.FindCachedResponse(cacheKey, g.cfg.Cache.MaxAge)
			if err != nil {
				logError("cache lookup failed: %v", err)
			}
			if cached != nil {
				return forwardResult{Cached: cached}
			}
		}

		resp, duration, err := g.forwardUpstream(r, bytes.NewReader(body), provider)
		totalDuration += duration

		if err == nil && resp.StatusCode < 500 {
			if i > 0 {
				logInfo("failover: provider[%d] (%s) succeeded", i, provider.BaseURL)
			}
			return forwardResult{Resp: resp, Duration: totalDuration}
		}

		if resp != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("provider[%d] returned %d", i, resp.StatusCode)
		} else {
			lastErr = err
		}

		if i < len(providers)-1 {
			logInfo("failover: provider[%d] (%s) failed: %v, trying provider[%d] (%s)",
				i, provider.BaseURL, lastErr, i+1, providers[i+1].BaseURL)
		}
	}

	return forwardResult{Duration: totalDuration, Err: fmt.Errorf("all %d providers failed, last: %w", len(providers), lastErr)}
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
	applyProviderHeaders(upstreamRequest.Header, provider)

	startedAt := time.Now()
	resp, err := g.client.Do(upstreamRequest)
	duration := time.Since(startedAt)
	if err != nil {
		return nil, duration, fmt.Errorf("send upstream request: %w", err)
	}

	return resp, duration, nil
}

func applyProviderHeaders(headers http.Header, provider ProviderConfig) {
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
		if isProviderHeader(key, provider) {
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

func isProviderHeader(key string, provider ProviderConfig) bool {
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
