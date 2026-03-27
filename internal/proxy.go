package aimenshen

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	cfg      Config
	provider ProviderConfig
	storage  *Storage
	client   *http.Client
}

func NewGateway(cfg Config, storage *Storage) (*Gateway, error) {
	provider := cfg.PrimaryProvider()
	timeout := time.Duration(cfg.HTTPClient.Timeout) * time.Second
	service := &Gateway{
		cfg:      cfg,
		provider: provider,
		storage:  storage,
		client:   &http.Client{Transport: http.DefaultTransport, Timeout: timeout},
	}

	return service, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Optional auth check for all routes (Proxy, Dashboard, Reports, Assets)
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

	if r.Method == http.MethodGet {
		if r.URL.Path == "/" {
			g.handleDashboard(w)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			g.handleAssets(w, r)
			return
		}

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

	startedAt := time.Now()

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	meta, err := AnalyzeRequest(r.URL.Path, requestBody, g.provider)
	if err != nil {
		log.Printf("failed to analyze request: %v", err)
		// Fallback to original body and default meta
		meta = RequestMeta{
			EffectiveBody: requestBody,
		}
	}

	if g.cfg.Verbose && len(meta.EffectiveBody) > 0 {
		log.Printf("Request Body: %s", string(meta.EffectiveBody))
	}

	requestLog := RequestLog{
		ID:          newRequestID(),
		CreatedAt:   startedAt,
		Path:        r.URL.Path,
		Model:       meta.EffectiveModel,
		CacheKey:    meta.CacheKey,
		RequestBody: g.requestBodyForStorage(meta),
	}

	if meta.Stream {
		g.proxyStream(w, r, meta, requestLog)
		return
	}

	if canUseCache(r, meta, g.cfg.Cache) {
		cached, err := g.storage.FindCachedResponse(meta.CacheKey, g.cfg.Cache.MaxBodyBytes, g.cfg.Cache.MaxAge)
		if err != nil {
			log.Printf("cache lookup failed: %v", err)
		}
		if cached != nil {
			g.serveCachedResponse(w, r, startedAt, requestLog, cached)
			return
		}
	}

	resp, duration, err := g.forwardUpstream(r, meta.EffectiveBody)
	if err != nil {
		log.Printf("upstream request failed: %v", err)
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
		log.Printf("failed to read upstream response: %v", err)
		g.saveExchange(requestLog, ResponseLog{
			RequestID:  requestLog.ID,
			StatusCode: http.StatusBadGateway,
			DurationMS: duration.Milliseconds(),
		}, nil)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	if g.cfg.Verbose && len(responseBody) > 0 {
		log.Printf("Response Body: %s", string(responseBody))
	}

	usage, err := ExtractUsage(requestLog.ID, responseBody)
	if err != nil {
		log.Printf("usage extraction failed: %v", err)
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
		log.Printf("write response failed: %v", err)
	}

	log.Printf("[%d] %s %s (%.3fs)", resp.StatusCode, r.Method, r.URL.String(), duration.Seconds())
}

func (g *Gateway) proxyStream(w http.ResponseWriter, r *http.Request, meta RequestMeta, requestLog RequestLog) {
	resp, duration, err := g.forwardUpstream(r, meta.EffectiveBody)
	if err != nil {
		log.Printf("stream upstream request failed: %v", err)
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
	if g.cfg.Logging.LogResponseBody || g.cfg.Verbose {
		captured = &bytes.Buffer{}
	}
	// Use a fixed sized buffer for streaming to keep memory overhead predictable
	buffer := make([]byte, 16*1024)

	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if _, err := w.Write(chunk); err != nil {
				log.Printf("stream response write failed: %v", err)
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			if err := usageExtractor.Write(chunk); err != nil {
				log.Printf("stream usage extraction failed: %v", err)
				// Do not break, keep proxying the stream
			}
			if captured != nil {
				// Don't capture more than 1MB to avoid memory blow-up
				if captured.Len() < 1024*1024 {
					_, _ = captured.Write(chunk)
				}
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Printf("stream response copy failed: %v", readErr)
			break
		}
	}

	elapsed := duration
	if total := time.Since(requestLog.CreatedAt); total > elapsed {
		elapsed = total
	}

	if err := usageExtractor.Finalize(); err != nil {
		log.Printf("stream usage extraction failed: %v", err)
	}

	if captured != nil && captured.Len() > 0 {
		if g.cfg.Verbose {
			log.Printf("Stream Response Body: %s", captured.String())
		}
	}

	responseBody := ""
	if captured != nil {
		responseBody = g.streamResponseBodyForStorage(captured.Bytes())
	}

	responseLog := ResponseLog{
		RequestID:    requestLog.ID,
		StatusCode:   resp.StatusCode,
		ResponseBody: responseBody,
		DurationMS:   elapsed.Milliseconds(),
	}
	g.saveExchange(requestLog, responseLog, usageExtractor.Usage())

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		log.Printf("stream response drain failed: %v", err)
	}

	log.Printf("[%d] %s %s (%.3fs)", resp.StatusCode, r.Method, r.URL.String(), elapsed.Seconds())
}

func (g *Gateway) serveCachedResponse(w http.ResponseWriter, r *http.Request, startedAt time.Time, requestLog RequestLog, cached *CachedResponse) {
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

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(cached.StatusCode)
	if _, err := io.WriteString(w, cached.ResponseBody); err != nil {
		log.Printf("write cached response failed: %v", err)
	}

	log.Printf("[%d] %s %s (%.3fs, cache hit)", cached.StatusCode, r.Method, r.URL.String(), duration.Seconds())
}

func (g *Gateway) handleModelReport(w http.ResponseWriter, r *http.Request) {
	days := g.getDays(r)
	reports, err := g.storage.ModelUsageReports(days)
	if err != nil {
		log.Printf("model report error: %v", err)
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
		log.Printf("summary report error: %v", err)
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
		log.Printf("daily report error: %v", err)
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
		log.Printf("logs report error: %v", err)
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
		log.Printf("log detail error: %v", err)
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

func (g *Gateway) forwardUpstream(r *http.Request, body []byte) (*http.Response, time.Duration, error) {
	targetURL, err := buildUpstreamURL(g.provider.BaseURL, r.URL)
	if err != nil {
		return nil, 0, err
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create upstream request: %w", err)
	}

	g.copyRequestHeaders(upstreamRequest.Header, r.Header)
	g.applyProviderHeaders(upstreamRequest.Header)

	startedAt := time.Now()
	resp, err := g.client.Do(upstreamRequest)
	duration := time.Since(startedAt)
	if err != nil {
		return nil, duration, fmt.Errorf("send upstream request: %w", err)
	}

	return resp, duration, nil
}

func (g *Gateway) applyProviderHeaders(headers http.Header) {
	// 1. Always clear the client's standard Authorization
	headers.Del(authHeaderName)

	// 2. Apply APIKey if present
	if g.provider.APIKey != "" {
		headers.Set(authHeaderName, "Bearer "+g.provider.APIKey)
	}

	// 3. Apply custom headers (can override Authorization if specified)
	for k, v := range g.provider.Headers {
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

func (g *Gateway) copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		if g.isProviderHeader(key) {
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

func (g *Gateway) isProviderHeader(key string) bool {
	if strings.EqualFold(key, authHeaderName) {
		return true
	}
	for k := range g.provider.Headers {
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

func (g *Gateway) requestBodyForStorage(meta RequestMeta) string {
	if !g.cfg.Logging.LogRequestBody {
		return ""
	}
	return string(meta.EffectiveBody)
}

func (g *Gateway) responseBodyForStorage(r *http.Request, meta RequestMeta, statusCode int, responseBody []byte) string {
	if g.cfg.Logging.LogResponseBody || canStoreCachedResponse(r, meta, statusCode, responseBody, g.cfg.Cache) {
		return string(responseBody)
	}
	return ""
}

func (g *Gateway) streamResponseBodyForStorage(responseBody []byte) string {
	if !g.cfg.Logging.LogResponseBody {
		return ""
	}
	return string(responseBody)
}

func (g *Gateway) cachedResponseBodyForStorage(responseBody string) string {
	if g.cfg.Logging.LogResponseBody {
		return responseBody
	}
	return ""
}

func (g *Gateway) saveExchange(requestLog RequestLog, responseLog ResponseLog, usage *UsageLog) {
	if err := g.storage.SaveExchange(requestLog, responseLog, usage); err != nil {
		log.Printf("save exchange failed: %v", err)
	}
}

func newRequestID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(bytes[:])
}
