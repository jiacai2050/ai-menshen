package aimenshen

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBuildCacheKeyExplicit(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		payload   string
		expectKey string
	}{
		{
			name:      "simple request",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","temp":0.7}`,
			expectKey: "v1:33a9d5599056bf6d61f3825366ec5924db8ee25bbab8c3fabcbd869c22f52f9b",
		},
		{
			name:      "same payload, different path",
			path:      "/v1/completions",
			payload:   `{"model":"gpt-4o","temp":0.7}`,
			expectKey: "v1:a02ac362ad0e02dcfd460c778028aad15fa1f58dc99506cc0534e90c81ec7861",
		},
		{
			name:      "different key order",
			path:      "/v1/chat/completions",
			payload:   `{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o"}`,
			expectKey: "v1:6a0e24eb3d08fc248852799017862dc6e19e0b323e3417735c63c90c45384762",
		},
		{
			name:      "different key order (reversed)",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			expectKey: "v1:6a0e24eb3d08fc248852799017862dc6e19e0b323e3417735c63c90c45384762",
		},
		{
			name:      "excluded fields (user)",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","messages":[],"user":"user-123"}`,
			expectKey: "v1:767d478c9349beda7e10ee0590b86fcaf57bc7ff48bd5c4229908834923e144f",
		},
		{
			name:      "excluded fields (stream_options)",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","messages":[],"stream_options":{"include_usage":true}}`,
			expectKey: "v1:767d478c9349beda7e10ee0590b86fcaf57bc7ff48bd5c4229908834923e144f",
		},
		{
			name:      "float consistency (verify 1)",
			path:      "/v1/chat/completions",
			payload:   `{"temp": 0.0000001, "top_p": 1}`,
			expectKey: "v1:156c816deb0adc2b84702ceffa009ce5b9ec83dd366b356147ad3df74f6f96ac",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := decodeJSONObject([]byte(tt.payload))
			if !ok {
				t.Fatalf("failed to decode payload: %s", tt.payload)
			}

			got, err := buildCacheKey(tt.path, p)
			if err != nil {
				t.Fatalf("buildCacheKey() error = %v", err)
			}
			if tt.expectKey != "" && got != tt.expectKey {
				t.Errorf("buildCacheKey() got = %s, want %s", got, tt.expectKey)
			}
		})
	}
}

func TestParseRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     []byte
		expected RequestMeta
	}{
		{
			name:     "empty body",
			expected: RequestMeta{},
		},
		{
			name: "non-json passthrough",
			body: []byte("not json"),
			expected: RequestMeta{
				OriginalBody: []byte("not json"),
			},
		},
		{
			name: "basic model extraction",
			body: []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`),
			expected: RequestMeta{
				OriginalBody:   []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`),
				EffectiveModel: "gpt-4o-mini",
			},
		},
		{
			name: "stream detection",
			body: []byte(`{"model":"gpt-4o","messages":[],"stream":true}`),
			expected: RequestMeta{
				OriginalBody:   []byte(`{"model":"gpt-4o","messages":[],"stream":true}`),
				EffectiveModel: "gpt-4o",
				Stream:         true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := ParseRequest(tt.body)

			if meta.EffectiveModel != tt.expected.EffectiveModel {
				t.Errorf("EffectiveModel = %s, want %s", meta.EffectiveModel, tt.expected.EffectiveModel)
			}
			if meta.Stream != tt.expected.Stream {
				t.Errorf("Stream = %v, want %v", meta.Stream, tt.expected.Stream)
			}
			if !reflect.DeepEqual(meta.OriginalBody, tt.expected.OriginalBody) {
				t.Errorf("OriginalBody = %s, want %s", meta.OriginalBody, tt.expected.OriginalBody)
			}
		})
	}
}

func TestPrepareForProvider(t *testing.T) {
	tests := []struct {
		name          string
		body          []byte
		provider      ProviderConfig
		expectedModel string
		expectedBody  []byte
		hasCacheKey   bool
	}{
		{
			name:          "no override",
			body:          []byte(`{"model":"gpt-4o","messages":[]}`),
			provider:      ProviderConfig{},
			expectedModel: "gpt-4o",
			expectedBody:  []byte(`{"messages":[],"model":"gpt-4o"}`),
			hasCacheKey:   true,
		},
		{
			name:          "model override",
			body:          []byte(`{"model":"gpt-4o-mini","messages":[]}`),
			provider:      ProviderConfig{Model: "gpt-4.1"},
			expectedModel: "gpt-4.1",
			expectedBody:  []byte(`{"messages":[],"model":"gpt-4.1"}`),
			hasCacheKey:   true,
		},
		{
			name:          "non-json passthrough",
			body:          []byte("not json"),
			provider:      ProviderConfig{Model: "gpt-4.1"},
			expectedModel: "",
			expectedBody:  []byte("not json"),
			hasCacheKey:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := ParseRequest(tt.body)
			body, cacheKey, model := PrepareForProvider("/v1/chat/completions", meta, tt.provider)

			body = canonicalBody(t, body)
			expectedBody := canonicalBody(t, tt.expectedBody)

			if !reflect.DeepEqual(body, expectedBody) {
				t.Errorf("body = %s, want %s", body, expectedBody)
			}
			if model != tt.expectedModel {
				t.Errorf("model = %s, want %s", model, tt.expectedModel)
			}
			if tt.hasCacheKey && cacheKey == "" {
				t.Error("expected non-empty cache key")
			}
			if !tt.hasCacheKey && cacheKey != "" {
				t.Errorf("expected empty cache key, got %s", cacheKey)
			}
		})
	}
}

func TestPrepareForProviderCacheKeyDiffers(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	meta := ParseRequest(body)

	_, key1, _ := PrepareForProvider("/chat/completions", meta, ProviderConfig{Model: "gpt-4o"})
	_, key2, _ := PrepareForProvider("/chat/completions", meta, ProviderConfig{Model: "gpt-4.1"})

	if key1 == "" || key2 == "" {
		t.Fatal("expected non-empty cache keys")
	}
	if key1 == key2 {
		t.Errorf("cache keys should differ for different models, got %s", key1)
	}
}

func canonicalBody(t *testing.T, body []byte) []byte {
	t.Helper()

	if len(body) == 0 {
		return nil
	}

	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}

	canonical, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("canonicalBody() error = %v", err)
	}

	return canonical
}
