package aimenshen

import (
	"bytes"
	"net/http"
)

var cacheablePaths = map[string]struct{}{
	"/chat/completions": {},
	"/responses":        {},
}

// sseDoneMarker is the OpenAI-style end-of-stream marker for SSE responses.
var sseDoneMarker = []byte("data: [DONE]")

func canUseCache(r *http.Request, meta RequestMeta, cacheConfig CacheConfig) bool {
	if !cacheConfig.Enable || meta.Stream || meta.CacheKey == "" {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	if _, ok := cacheablePaths[r.URL.Path]; !ok {
		return false
	}
	return true
}

// canUseCacheForStream returns true when a streaming request is eligible for
// cache lookup or storage (same conditions as canUseCache but for stream=true).
func canUseCacheForStream(r *http.Request, meta RequestMeta, cacheConfig CacheConfig) bool {
	if !cacheConfig.Enable || !meta.Stream || meta.CacheKey == "" {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	if _, ok := cacheablePaths[r.URL.Path]; !ok {
		return false
	}
	return true
}

// isStreamBodyComplete reports whether a captured SSE body contains the
// OpenAI end-of-stream marker "data: [DONE]", indicating a complete response.
func isStreamBodyComplete(body []byte) bool {
	return bytes.Contains(body, sseDoneMarker)
}

func canStoreCachedResponse(r *http.Request, meta RequestMeta, statusCode int, responseBody []byte, cacheConfig CacheConfig) bool {
	if !canUseCache(r, meta, cacheConfig) {
		return false
	}
	if statusCode != http.StatusOK {
		return false
	}
	if cacheConfig.MaxBodyBytes > 0 && int64(len(responseBody)) > cacheConfig.MaxBodyBytes {
		return false
	}
	return true
}

// canStoreCachedStreamResponse returns true when a captured stream body should
// be written to the cache. Requirements: eligible path, status 200, body
// contains the SSE done marker, and body size is within the configured limit.
func canStoreCachedStreamResponse(r *http.Request, meta RequestMeta, statusCode int, responseBody []byte, cacheConfig CacheConfig) bool {
	if !canUseCacheForStream(r, meta, cacheConfig) {
		return false
	}
	if statusCode != http.StatusOK {
		return false
	}
	if !isStreamBodyComplete(responseBody) {
		return false
	}
	if cacheConfig.MaxBodyBytes > 0 && int64(len(responseBody)) > cacheConfig.MaxBodyBytes {
		return false
	}
	return true
}
