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
	"strings"
	"time"
)

const (
	authHeaderName   = "Authorization"
	cfAuthHeaderName = "cf-aig-authorization"
	reportModelsPath = "/__report/models"
)

type Gateway struct {
	cfg      Config
	provider ProviderConfig
	storage  *Storage
	client   *http.Client
}

func NewGateway(cfg Config, storage *Storage) (*Gateway, error) {
	service := &Gateway{
		cfg:      cfg,
		provider: cfg.PrimaryProvider(),
		storage:  storage,
		client:   &http.Client{Transport: http.DefaultTransport},
	}

	if cfg.Verbose {
		log.Printf("Using auth header %q", service.authHeaderName())
	}

	return service, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == reportModelsPath {
		g.handleModelReport(w)
		return
	}

	startedAt := time.Now()

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	meta, err := AnalyzeRequest(r.URL.Path, requestBody, g.provider)
	if err != nil {
		http.Error(w, "failed to analyze request", http.StatusBadRequest)
		return
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
		cached, err := g.storage.FindCachedResponse(meta.CacheKey, g.cfg.Cache.MaxBodyBytes)
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
	buffer := make([]byte, 32*1024)

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
			}
			if captured != nil {
				_, _ = captured.Write(chunk)
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

func (g *Gateway) handleModelReport(w http.ResponseWriter) {
	reports, err := g.storage.ModelUsageReports()
	if err != nil {
		http.Error(w, "failed to query model report", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reports); err != nil {
		log.Printf("write model report failed: %v", err)
	}
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

	copyRequestHeaders(upstreamRequest.Header, r.Header)
	applyProviderAuthHeader(upstreamRequest.Header, g.provider)

	startedAt := time.Now()
	resp, err := g.client.Do(upstreamRequest)
	duration := time.Since(startedAt)
	if err != nil {
		return nil, duration, fmt.Errorf("send upstream request: %w", err)
	}

	return resp, duration, nil
}

func (g *Gateway) authHeaderName() string {
	parsed, err := url.Parse(g.provider.BaseURL)
	if err != nil {
		return authHeaderName
	}
	if parsed.Host == "gateway.ai.cloudflare.com" {
		return cfAuthHeaderName
	}
	return authHeaderName
}

func applyProviderAuthHeader(headers http.Header, provider ProviderConfig) {
	headers.Del(authHeaderName)
	headers.Del(cfAuthHeaderName)
	headers.Set(providerAuthHeaderName(provider.BaseURL), "Bearer "+provider.APIKey)
}

func providerAuthHeaderName(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return authHeaderName
	}
	if parsed.Host == "gateway.ai.cloudflare.com" {
		return cfAuthHeaderName
	}
	return authHeaderName
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

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		if strings.EqualFold(key, authHeaderName) || strings.EqualFold(key, cfAuthHeaderName) {
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
