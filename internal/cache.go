package aimenshen

import (
	"bytes"
	"net/http"
)

var auditablePaths = map[string]struct{}{
	"/chat/completions": {},
	"/responses":        {},
}

func isAuditablePath(path string) bool {
	_, ok := auditablePaths[path]
	return ok
}

// sseDoneMarker is the OpenAI-style end-of-stream marker for SSE responses.
var sseDoneMarker = []byte("data: [DONE]")

func canUseCache(r *http.Request, meta RequestMeta, cacheConfig CacheConfig) bool {
	if !cacheConfig.Enable || meta.CacheKey == "" {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	if _, ok := auditablePaths[r.URL.Path]; !ok {
		return false
	}
	return true
}

// isStreamBodyComplete reports whether a captured SSE body ends with the
// OpenAI end-of-stream marker "data: [DONE]", indicating a complete response.
// We check for the suffix to avoid false positives if the marker appears
// within the model's generated content.
func isStreamBodyComplete(body []byte) bool {
	return bytes.HasSuffix(bytes.TrimSpace(body), sseDoneMarker)
}

func canStoreCachedResponse(r *http.Request, meta RequestMeta, statusCode int, responseBody []byte, cacheConfig CacheConfig) bool {
	if !canUseCache(r, meta, cacheConfig) {
		return false
	}
	if statusCode != http.StatusOK {
		return false
	}
	if meta.Stream && !isStreamBodyComplete(responseBody) {
		return false
	}
	if cacheConfig.MaxBodyBytes > 0 && int64(len(responseBody)) > cacheConfig.MaxBodyBytes {
		return false
	}
	return true
}
