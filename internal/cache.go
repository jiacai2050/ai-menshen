package menshen

import (
	"net/http"
)

func isAuditablePath(path string) bool {
	return findProtocol(path) != nil
}

func canUseCache(r *http.Request, meta RequestMeta, cacheConfig CacheConfig) bool {
	if !cacheConfig.Enable || meta.CacheKey == "" {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	return isAuditablePath(r.URL.Path)
}

func canStoreCachedResponse(r *http.Request, meta RequestMeta, statusCode int, responseBody []byte, streamComplete bool, cacheConfig CacheConfig) bool {
	if !canUseCache(r, meta, cacheConfig) {
		return false
	}
	if statusCode != http.StatusOK {
		return false
	}
	if meta.Stream && !streamComplete {
		return false
	}
	if cacheConfig.MaxBodyBytes > 0 && int64(len(responseBody)) > cacheConfig.MaxBodyBytes {
		return false
	}
	return true
}
