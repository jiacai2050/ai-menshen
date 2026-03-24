package aimenshen

import "net/http"

var cacheablePaths = map[string]struct{}{
	"/chat/completions": {},
	"/responses":        {},
}

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
