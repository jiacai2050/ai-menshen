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

func TestCanStoreCachedStreamResponse(t *testing.T) {
	validBody := []byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	noMarkerBody := []byte("data: {\"choices\":[]}\n\n")

	makeRequest := func(method, path string) *http.Request {
		req := httptest.NewRequest(method, path, nil)
		return req
	}

	streamMeta := RequestMeta{
		Stream:   true,
		CacheKey: "testkey",
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
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: true},
			expected: true,
		},
		{
			name:     "cache disabled → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: false},
			expected: false,
		},
		{
			name:     "non-stream meta → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "testkey"},
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "no cache key → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: ""},
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "non-200 status → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     streamMeta,
			status:   http.StatusInternalServerError,
			body:     validBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "missing DONE marker → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     noMarkerBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "body exceeds MaxBodyBytes → not stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: true, MaxBodyBytes: 10},
			expected: false,
		},
		{
			name:     "large body with no MaxBodyBytes limit → stored",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     append(make([]byte, 2*1024*1024), []byte("data: [DONE]\n\n")...),
			cfg:      CacheConfig{Enable: true, MaxBodyBytes: 0},
			expected: true,
		},
		{
			name:     "uncacheable path → not stored",
			r:        makeRequest(http.MethodPost, "/embeddings"),
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "GET method → not stored",
			r:        makeRequest(http.MethodGet, "/chat/completions"),
			meta:     streamMeta,
			status:   http.StatusOK,
			body:     validBody,
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canStoreCachedStreamResponse(tt.r, tt.meta, tt.status, tt.body, tt.cfg)
			if got != tt.expected {
				t.Errorf("canStoreCachedStreamResponse() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCanUseCacheForStream(t *testing.T) {
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
			name:     "stream POST to /responses → true",
			r:        makeRequest(http.MethodPost, "/responses"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: true,
		},
		{
			name:     "cache disabled → false",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			cfg:      CacheConfig{Enable: false},
			expected: false,
		},
		{
			name:     "non-stream → false",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: false, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "empty cache key → false",
			r:        makeRequest(http.MethodPost, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: ""},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "uncacheable path → false",
			r:        makeRequest(http.MethodPost, "/embeddings"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
		{
			name:     "GET method → false",
			r:        makeRequest(http.MethodGet, "/chat/completions"),
			meta:     RequestMeta{Stream: true, CacheKey: "key"},
			cfg:      CacheConfig{Enable: true},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canUseCacheForStream(tt.r, tt.meta, tt.cfg)
			if got != tt.expected {
				t.Errorf("canUseCacheForStream() = %v, want %v", got, tt.expected)
			}
		})
	}
}

