package menshen

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jiacai2050/menshen/internal/web"
)

const (
	authHeaderName      = "Authorization"
	providerPathPrefix  = "/provider/"
	providerPathRoot    = "/provider"
	reportModelsPath    = "/__report/models"
	reportProvidersPath = "/__report/providers"
	reportSummaryPath   = "/__report/summary"
	reportDailyPath     = "/__report/daily"
	reportLogsPath      = "/__report/logs"
	reportLogDetailPath = "/__report/log"
)

const statusClientClosedRequest = 499

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
	bases             map[string]*url.URL     // keyed by provider base URL, parsed once at startup
}

func NewGateway(cfg Config, storage *Storage) (*Gateway, error) {
	activeProviders, activeTotalWeight := buildActiveProviders(cfg.Providers)
	if len(activeProviders) == 0 || activeTotalWeight <= 0 {
		return nil, fmt.Errorf("gateway requires at least one active provider")
	}

	timeout := time.Duration(cfg.Upstream.Timeout) * time.Second
	clients := make(map[string]*http.Client)
	bases := make(map[string]*url.URL, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if _, exists := bases[p.BaseURL]; !exists {
			base, err := url.Parse(p.BaseURL)
			if err != nil {
				return nil, fmt.Errorf("parse provider base URL %q: %w", p.BaseURL, err)
			}
			bases[p.BaseURL] = base
		}
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
		bases:             bases,
	}

	return service, nil
}

// buildActiveProviders filters out providers with non-positive weight and
// orders the rest for failover: heavier providers first, ties broken by name
// so the order is deterministic across restarts.
func buildActiveProviders(providers map[string]ProviderConfig) ([]ProviderConfig, int) {
	activeProviders := make([]ProviderConfig, 0, len(providers))
	totalWeight := 0
	for _, provider := range providers {
		if provider.Weight <= 0 {
			continue
		}
		activeProviders = append(activeProviders, provider)
		totalWeight += provider.Weight
	}

	slices.SortFunc(activeProviders, func(a, b ProviderConfig) int {
		if a.Weight != b.Weight {
			return b.Weight - a.Weight
		}
		return strings.Compare(a.Name, b.Name)
	})

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

func (g *Gateway) providersForCapability(capName string) []ProviderConfig {
	var matched []ProviderConfig
	for _, p := range g.activeProviders {
		if p.HasCapability(capName) {
			matched = append(matched, p)
		}
	}
	return matched
}

func (g *Gateway) pickProviderForCapability(capName string) (ProviderConfig, error) {
	candidates := g.providersForCapability(capName)
	if len(candidates) == 0 {
		return ProviderConfig{}, fmt.Errorf("no active provider configured for capability %q", capName)
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	totalWeight := 0
	for _, p := range candidates {
		totalWeight += p.Weight
	}
	if totalWeight <= 0 {
		return candidates[0], nil
	}

	pick := mrand.IntN(totalWeight)
	for _, provider := range candidates {
		pick -= provider.Weight
		if pick < 0 {
			return provider, nil
		}
	}

	return candidates[len(candidates)-1], nil
}

// resolveProvider determines the provider to handle the request and the path
// that should be used upstream.
//
// If path starts with "/provider/<provider>/", the named provider is selected
// explicitly (disabling failover) and the returned path has the
// "/provider/<provider>" prefix stripped. Otherwise an active provider is
// picked by weighted random and path is returned unchanged. The request itself
// is never modified, so resolveProvider stays idempotent and callers decide
// when to apply the rewritten path.
func (g *Gateway) resolveProvider(path string) (ProviderConfig, string, bool, error) {
	if path == providerPathRoot {
		return ProviderConfig{}, path, false, fmt.Errorf("missing provider name: use %s<name>/...", providerPathPrefix)
	}
	if !strings.HasPrefix(path, providerPathPrefix) {
		return ProviderConfig{}, path, false, nil
	}

	provName, remainingPath, found := strings.Cut(path[len(providerPathPrefix):], "/")
	if provName == "" {
		return ProviderConfig{}, path, false, fmt.Errorf("missing provider name: use %s<name>/...", providerPathPrefix)
	}
	if !found {
		// Bare "/provider/<name>" targets the upstream root.
		remainingPath = "/"
	} else {
		remainingPath = "/" + remainingPath
	}

	provider, ok := g.cfg.Providers[provName]
	if !ok {
		return ProviderConfig{}, path, false, fmt.Errorf("unknown provider %q", provName)
	}

	return provider, remainingPath, true, nil
}

// shouldFailover returns true if the upstream response indicates we should
// try the next provider: network errors, HTTP 5xx, or select 4xx codes
// where a different provider may succeed.
func shouldFailover(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, // 401
		http.StatusForbidden,             // 403
		http.StatusNotFound,              // 404
		http.StatusRequestTimeout,        // 408
		http.StatusGone,                  // 410
		http.StatusRequestEntityTooLarge, // 413
		http.StatusUnprocessableEntity,   // 422
		http.StatusTooManyRequests:       // 429
		return true
	default:
		return resp.StatusCode >= 500
	}
}

func (g *Gateway) ServeWebHTTP(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Auth.Enable {
		user, pass, ok := r.BasicAuth()
		if !ok || user != g.cfg.Auth.User || pass != g.cfg.Auth.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="menshen"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	if r.Method == http.MethodGet {
		if r.URL.Path == "/" || r.URL.Path == "/overview" {
			g.handleOverview(w)
			return
		}
		if r.URL.Path == "/logs" || strings.HasPrefix(r.URL.Path, "/logs/") {
			g.handleLogsPage(w)
			return
		}
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
		case reportProvidersPath:
			g.handleProviderReport(w, r)
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

	http.NotFound(w, r)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.ServeAPIHTTP(w, r)
}

func (g *Gateway) ServeAPIHTTP(w http.ResponseWriter, r *http.Request) {
	if g.cfg.Auth.Enable {
		authHeader := r.Header.Get(authHeaderName)
		apiKeyHeader := r.Header.Get("x-api-key")
		expected := "Bearer " + g.cfg.Auth.Token
		if authHeader != expected && apiKeyHeader != g.cfg.Auth.Token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	provider, path, explicit, err := g.resolveProvider(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if path != r.URL.Path {
		// The provider prefix was stripped, so the raw (escaped) path no longer
		// encodes r.URL.Path and has to be dropped.
		r.URL.Path = path
		r.URL.RawPath = ""
	}

	proto := findProtocol(r.URL.Path)
	if proto == nil {
		if !explicit {
			provider = g.pickProvider()
		}
		g.proxyPassthrough(w, r, provider)
		return
	}

	startedAt := time.Now()

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	// For unpinned requests, select capability-compatible provider BEFORE PrepareBody
	// so that model overrides and cache key correspond to the actual provider.
	if !explicit {
		picked, err := g.pickProviderForCapability(proto.Name())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		provider = picked
	}

	meta, err := proto.PrepareBody(r.URL.Path, requestBody, provider, g.cfg.Cache.Enable)
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
		Provider:    provider.Name,
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

	// Cache missed: validate capability for explicitly selected provider
	if explicit {
		if !provider.HasCapability(proto.Name()) {
			http.Error(w, fmt.Sprintf("provider %q does not support capability %q", provider.Name, proto.Name()), http.StatusBadRequest)
			return
		}
	}

	if meta.Stream {
		g.proxyStream(w, r, requestBody, meta, &requestLog, provider, !explicit, proto)
		return
	}

	// Non-stream: try provider, failover to remaining active providers on failure (if not explicitly requested)
	resp, meta, err := g.forwardWithFailover(r, requestBody, &requestLog, provider, meta, !explicit)
	if resp == nil {
		g.respondBadGateway(w, r, requestLog, err)
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		elapsed := time.Since(startedAt)
		cause := "read upstream response: " + failureCause(err)
		logError("[%s] [%s] [%d] %s %s (%.3fs): %s", requestLog.ID, requestLog.Provider, http.StatusBadGateway, r.Method, r.URL.String(), elapsed.Seconds(), cause)
		requestLog.CacheKey = ""
		g.saveExchange(requestLog, ResponseLog{
			RequestID:    requestLog.ID,
			StatusCode:   http.StatusBadGateway,
			ResponseBody: errorBody(cause),
			DurationMS:   elapsed.Milliseconds(),
		}, nil)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	if g.cfg.Verbose && len(responseBody) > 0 {
		logInfo("Response Body: %s", responseBody)
	}

	usage, err := proto.ExtractUsage(requestLog.ID, responseBody)
	if err != nil {
		logError("[%s] [%s] usage extraction failed: %v", requestLog.ID, requestLog.Provider, err)
	}

	canCache := canStoreCachedResponse(r, meta, resp.StatusCode, responseBody, true, g.cfg.Cache)
	if !canCache {
		requestLog.CacheKey = ""
	}

	elapsed := time.Since(startedAt)
	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   resp.StatusCode,
		ResponseBody: g.responseBodyForStorage(responseBody, canCache),
		DurationMS:   elapsed.Milliseconds(),
	}
	g.saveExchange(requestLog, responseLog, usage)

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(responseBody); err != nil {
		logError("[%s] [%s] write response failed: %v", requestLog.ID, requestLog.Provider, err)
	}

	logInfo("[%s] [%s] [%d] %s %s (%.3fs)", requestLog.ID, requestLog.Provider, resp.StatusCode, r.Method, r.URL.String(), elapsed.Seconds())
}

func (g *Gateway) proxyStream(w http.ResponseWriter, r *http.Request, requestBody []byte, meta RequestMeta, requestLog *RequestLog, provider ProviderConfig, canFailover bool, proto Protocol) {
	resp, meta, err := g.forwardWithFailover(r, requestBody, requestLog, provider, meta, canFailover)
	if resp == nil {
		g.respondBadGateway(w, r, *requestLog, err)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	auditor := proto.NewStreamAuditor(requestLog.ID)
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

	var readFailed, writeFailed, clientCanceled, capturedTruncated bool
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if _, err := w.Write(chunk); err != nil {
				if errors.Is(r.Context().Err(), context.Canceled) {
					logInfo("[%s] [%s] client canceled stream during write", requestLog.ID, requestLog.Provider)
					clientCanceled = true
				} else {
					logError("[%s] [%s] stream response write failed: %v", requestLog.ID, requestLog.Provider, err)
					writeFailed = true
				}
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			if err := auditor.Write(chunk); err != nil {
				logError("[%s] [%s] stream usage extraction failed: %v", requestLog.ID, requestLog.Provider, err)
				// Do not break, keep proxying the stream
			}
			if captured != nil {
				if limit == 0 {
					_, _ = captured.Write(chunk)
				} else if int64(captured.Len()+len(chunk)) <= limit {
					_, _ = captured.Write(chunk)
				} else {
					capturedTruncated = true
				}
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
				logInfo("[%s] [%s] client canceled stream: %v", requestLog.ID, requestLog.Provider, readErr)
				clientCanceled = true
			} else {
				logError("[%s] [%s] stream response copy failed: %v", requestLog.ID, requestLog.Provider, readErr)
				readFailed = true
			}
			break
		}
	}

	elapsed := time.Since(requestLog.CreatedAt)

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
	if clientCanceled {
		statusCode = statusClientClosedRequest
	} else if readFailed || writeFailed {
		statusCode = http.StatusBadGateway
	}

	isStreamComplete := auditor.IsComplete() && !capturedTruncated
	canCache := canStoreCachedResponse(r, meta, statusCode, capturedBytes, isStreamComplete, g.cfg.Cache)
	if !canCache {
		requestLog.CacheKey = ""
	}
	responseBody := g.responseBodyForStorage(capturedBytes, canCache)

	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   statusCode,
		ResponseBody: responseBody,
		DurationMS:   elapsed.Milliseconds(),
	}
	g.saveExchange(*requestLog, responseLog, auditor.Usage())

	logInfo("[%s] [%s] [%d] %s %s (%.3fs)", requestLog.ID, requestLog.Provider, statusCode, r.Method, r.URL.String(), elapsed.Seconds())
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
		logError("[%s] [%s] write cached response failed: %v", requestLog.ID, requestLog.Provider, err)
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
	logInfo("[%s] [%s] [%d] %s %s (%.3fs, %s)", requestLog.ID, requestLog.Provider, cached.StatusCode, r.Method, r.URL.String(), duration.Seconds(), logMsg)
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

func (g *Gateway) handleProviderReport(w http.ResponseWriter, r *http.Request) {
	days := g.getDays(r)
	reports, err := g.storage.ProviderUsageReports(days)
	if err != nil {
		logError("provider report error: %v", err)
		http.Error(w, "failed to query provider report", http.StatusInternalServerError)
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

func (g *Gateway) handleOverview(w http.ResponseWriter) {
	content, err := web.Assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "overview page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (g *Gateway) handleLogsPage(w http.ResponseWriter) {
	content, err := web.Assets.ReadFile("logs.html")
	if err != nil {
		http.Error(w, "logs page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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

func (g *Gateway) proxyPassthrough(w http.ResponseWriter, r *http.Request, provider ProviderConfig) {
	startedAt := time.Now()
	resp, _, err := g.forwardUpstream(r, r.Body, provider)
	if err != nil {
		logError("[passthrough] [%s] upstream request failed: %v", provider.Name, err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logError("[passthrough] [%s] response copy failed: %v", provider.Name, err)
	}

	duration := time.Since(startedAt)
	logInfo("[passthrough] [%s] [%d] %s %s (%.3fs)", provider.Name, resp.StatusCode, r.Method, r.URL.String(), duration.Seconds())
}

// drainLimit caps how much of a failed upstream body is drained before the
// connection is dropped, so a huge error body cannot stall a request.
const drainLimit = 64 << 10

func drainAndCloseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, drainLimit)
	resp.Body.Close()
}

// failureCause describes why an upstream attempt failed, for logs and storage.
func failureCause(err error) string {
	if err == nil {
		return "unknown upstream error"
	}
	return err.Error()
}

type errorResponseBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
}

// errorBody wraps a failure cause in a JSON body so the stored exchange keeps
// the upstream error visible after the request is gone.
func errorBody(cause string) []byte {
	body, err := json.Marshal(errorResponseBody{
		Error: errorDetail{Message: cause},
	})
	if err != nil {
		return nil
	}
	return body
}

type attemptResult struct {
	resp     *http.Response
	meta     RequestMeta
	provider ProviderConfig
	err      error
}

func (g *Gateway) forwardWithFailover(
	r *http.Request,
	requestBody []byte,
	requestLog *RequestLog,
	primary ProviderConfig,
	primaryMeta RequestMeta,
	allowFailover bool,
) (*http.Response, RequestMeta, error) {
	candidates := []ProviderConfig{primary}
	proto := findProtocol(r.URL.Path)
	capName := ""
	if proto != nil {
		capName = proto.Name()
	}
	if allowFailover && g.cfg.Failover.Enable && len(g.activeProviders) > 1 {
		for _, p := range g.activeProviders {
			if p.Name != primary.Name {
				if capName == "" || p.HasCapability(capName) {
					candidates = append(candidates, p)
				}
			}
		}
	}

	var (
		lastHTTP *attemptResult
		lastErr  *attemptResult
	)

	for i, p := range candidates {
		meta := primaryMeta
		if i > 0 {
			if r.Context().Err() != nil {
				// Client has disconnected or timed out; do not attempt remaining candidates.
				break
			}
			var err error
			if proto != nil {
				meta, err = proto.PrepareBody(r.URL.Path, requestBody, p, g.cfg.Cache.Enable)
			} else {
				meta, err = AnalyzeRequest(r.URL.Path, requestBody, p, g.cfg.Cache.Enable)
			}
			if err != nil {
				logError("failed to analyze request for failover provider: %v", err)
				meta = RequestMeta{EffectiveBody: requestBody}
			}
			logInfo("[%s] failover to provider %s (attempt %d/%d)", requestLog.ID, p.Name, i+1, len(candidates))
		}

		resp, duration, err := g.forwardUpstream(r, bytes.NewReader(meta.EffectiveBody), p)
		att := &attemptResult{
			resp:     resp,
			meta:     meta,
			provider: p,
			err:      err,
		}

		if err != nil {
			lastErr = att
			logError("[%s] upstream request failed for %s (%.3fs): %v", requestLog.ID, p.Name, duration.Seconds(), err)
			continue
		}

		if !shouldFailover(resp, nil) {
			// Success
			if lastHTTP != nil {
				drainAndCloseBody(lastHTTP.resp)
			}
			g.applyLogMetadata(requestLog, att.provider, att.meta)
			return resp, meta, nil
		}

		logError("[%s] upstream returned %d from %s (%.3fs)", requestLog.ID, resp.StatusCode, p.Name, duration.Seconds())
		if lastHTTP != nil {
			drainAndCloseBody(lastHTTP.resp)
		}
		lastHTTP = att
	}

	// Prefer returning the last received HTTP response (e.g. 5xx/429) so caller can pass it through.
	chosen := lastHTTP
	if chosen == nil {
		chosen = lastErr
	}
	if chosen != nil {
		g.applyLogMetadata(requestLog, chosen.provider, chosen.meta)
		return chosen.resp, chosen.meta, chosen.err
	}

	return nil, primaryMeta, errors.New("failover exhausted without any attempt")
}

func (g *Gateway) applyLogMetadata(requestLog *RequestLog, provider ProviderConfig, meta RequestMeta) {
	requestLog.Model = meta.EffectiveModel
	requestLog.Provider = provider.Name
	requestLog.CacheKey = meta.CacheKey
	requestLog.RequestBody = g.requestBodyForStorage(meta)
}

func (g *Gateway) respondBadGateway(w http.ResponseWriter, r *http.Request, requestLog RequestLog, err error) {
	duration := time.Since(requestLog.CreatedAt)
	cause := failureCause(err)
	logError("[%s] [%s] [%d] %s %s (%.3fs): %s", requestLog.ID, requestLog.Provider, http.StatusBadGateway, r.Method, r.URL.String(), duration.Seconds(), cause)
	requestLog.CacheKey = ""
	g.saveExchange(requestLog, ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   http.StatusBadGateway,
		ResponseBody: errorBody(cause),
		DurationMS:   duration.Milliseconds(),
	}, nil)
	http.Error(w, "upstream request failed", http.StatusBadGateway)
}

func (g *Gateway) forwardUpstream(r *http.Request, body io.Reader, provider ProviderConfig) (*http.Response, time.Duration, error) {
	base, ok := g.bases[provider.BaseURL]
	if !ok {
		return nil, 0, fmt.Errorf("no upstream base URL registered for %q", provider.BaseURL)
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, joinUpstreamURL(base, r.URL), body)
	if err != nil {
		return nil, 0, fmt.Errorf("create upstream request: %w", err)
	}

	g.copyRequestHeaders(upstreamRequest.Header, r.Header, provider)
	g.applyProviderHeaders(upstreamRequest.Header, provider, r.URL.Path)

	startedAt := time.Now()
	resp, err := g.clientForProvider(provider).Do(upstreamRequest)
	duration := time.Since(startedAt)
	if err != nil {
		return nil, duration, fmt.Errorf("send upstream request: %w", err)
	}

	return resp, duration, nil
}

func isAnthropicPath(path string) bool {
	p := strings.TrimPrefix(path, "/v1")
	return p == "/messages" || strings.HasPrefix(p, "/messages/") ||
		p == "/complete" || strings.HasPrefix(p, "/complete/")
}

func (g *Gateway) applyProviderHeaders(headers http.Header, provider ProviderConfig, path string) {
	proto := findProtocol(path)
	if proto != nil {
		proto.ApplyHeaders(headers, provider)
	} else if provider.APIKey != "" {
		// Passthrough path with provider API key configured
		if isAnthropicPath(path) || (provider.HasCapability(CapAnthropicMessages) && !provider.HasCapability(CapOpenAICompletions)) {
			// Anthropic target: requires x-api-key and rejects standard Authorization header
			headers.Del(authHeaderName)
			headers.Set("x-api-key", provider.APIKey)
			if headers.Get("anthropic-version") == "" {
				headers.Set("anthropic-version", "2023-06-01")
			}
		} else {
			// OpenAI or multi-capability general target: Bearer auth
			headers.Del(authHeaderName)
			headers.Set(authHeaderName, "Bearer "+provider.APIKey)
			if provider.HasCapability(CapAnthropicMessages) {
				// Inject x-api-key as well for dual-capability providers (e.g. OpenRouter / Cloudflare).
				// Most OpenAI-compatible endpoints ignore extra headers, while Anthropic endpoints accept x-api-key.
				headers.Set("x-api-key", provider.APIKey)
				if headers.Get("anthropic-version") == "" {
					headers.Set("anthropic-version", "2023-06-01")
				}
			}
		}
	}

	// Apply custom headers (can override any header if specified)
	for k, v := range provider.Headers {
		headers.Set(k, v)
	}
}

func joinUpstreamURL(base, incoming *url.URL) string {
	target := *base
	target.Path = joinURLPath(base.Path, incoming.Path)
	target.RawQuery = incoming.RawQuery
	return target.String()
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
		if g.isProviderHeader(key, values, provider) {
			continue
		}
		if strings.EqualFold(key, "Accept-Encoding") {
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

func (g *Gateway) isProviderHeader(key string, values []string, provider ProviderConfig) bool {
	if g.cfg.Auth.Enable {
		if strings.EqualFold(key, authHeaderName) {
			// Strip incoming Authorization because it was used to authenticate with this gateway,
			// preventing the gateway's token from leaking upstream.
			return true
		}
		if strings.EqualFold(key, "x-api-key") {
			// If incoming x-api-key matches the gateway token, strip it to prevent leakage upstream.
			for _, v := range values {
				if v == g.cfg.Auth.Token {
					return true
				}
			}
		}
	}
	if provider.APIKey != "" {
		if strings.EqualFold(key, authHeaderName) || strings.EqualFold(key, "x-api-key") {
			return true
		}
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

func (g *Gateway) responseBodyForStorage(responseBody []byte, canCache bool) []byte {
	if g.cfg.Logging.LogResponseBody || canCache {
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
	if g.storage == nil {
		return
	}
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
