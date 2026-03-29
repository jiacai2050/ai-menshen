package aimenshen

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsStreamBodyComplete(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		complete bool
	}{
		{
			name:     "empty body",
			body:     "",
			complete: false,
		},
		{
			name:     "body with DONE marker",
			body:     "data: {\"choices\":[]}\n\ndata: [DONE]\n\n",
			complete: true,
		},
		{
			name:     "body without DONE marker",
			body:     "data: {\"choices\":[]}\n\n",
			complete: false,
		},
		{
			name:     "partial DONE marker",
			body:     "data: [DON",
			complete: false,
		},
		{
			name:     "DONE marker embedded in longer body",
			body:     "data: chunk1\n\ndata: chunk2\n\ndata: [DONE]\n\n",
			complete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isStreamBodyComplete([]byte(tt.body))
			if got != tt.complete {
				t.Errorf("isStreamBodyComplete(%q) = %v, want %v", tt.body, got, tt.complete)
			}
		})
	}
}

func TestCanStoreCachedResponse(t *testing.T) {
	validStreamBody := []byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	validJSONBody := []byte("{\"choices\":[]}")

	makeRequest := func(method, path string) *http.Request {
		return httptest.NewRequest(method, path, nil)
	}

	tests := []struct {
		name     string
		r        *http.Request
		meta     RequestMeta
		status   int
		body     []byte
		cfg      CacheConfig
		expected bool
	}{
		{
			name:     "valid stream response stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			status:   http.StatusOK,
			body:     validStreamBody,
			cfg:      CacheConfig{Enable: true},
			expected: true,
		},
		{
			name:     "valid JSON response stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			status:   http.StatusOK,
			body:     validJSONBody,
			cfg:      CacheConfig{Enable: true},
			expected: true,
		},
		{
			name:     "stream missing DONE marker → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			status:   http.StatusOK,
			body:     validJSONBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "cache disabled → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			status:   http.StatusOK,
			body:     validJSONBody,
			cfg:      CacheConfig{Enable: false},
			expected: false,
		},
		{
			name:     "no cache key → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: ""},
			status:   http.StatusOK,
			body:     validJSONBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "non-200 status → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			status:   http.StatusInternalServerError,
			body:     validJSONBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "body exceeds MaxBodyBytes → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			status:   http.StatusOK,
			body:     validJSONBody,
			cfg:      CacheConfig{Enable: true, MaxBodyBytes: 5},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canStoreCachedResponse(tt.r, tt.meta, tt.status, tt.body, tt.cfg)
			if got != tt.expected {
				t.Errorf("%s: canStoreCachedResponse() = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestCanUseCache(t *testing.T) {
	makeRequest := func(method, path string) *http.Request {
		return httptest.NewRequest(method, path, nil)
	}

	tests := []struct {
		name     string
		r        *http.Request
		meta     RequestMeta
		cfg      CacheConfig
		expected bool
	}{
		{
			name:     "stream POST to cacheable path with key → true",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: true,
		},
		{
			name:     "JSON POST to cacheable path with key → true",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: true,
		},
		{
			name:     "cache disabled → false",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			cfg:      CacheConfig{Enable: false},
			expected: false,
		},
		{
			name:     "empty cache key → false",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: ""},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "uncacheable path → false",
			r:        makeRequest(http.MethodPost, "/embeddings"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "GET method → false",
			r:        makeRequest(http.MethodGet, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canUseCache(tt.r, tt.meta, tt.cfg)
			if got != tt.expected {
				t.Errorf("%s: canUseCache() = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}
