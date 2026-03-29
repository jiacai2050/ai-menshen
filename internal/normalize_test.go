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

func TestAnalyzeRequest(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		body     []byte
		provider ProviderConfig
		expected RequestMeta
	}{
		{
			name: "empty body",
			path: "/v1/chat/completions",
			expected: RequestMeta{
				EffectiveBody: nil,
			},
		},
		{
			name:     "non-json passthrough",
			path:     "/v1/chat/completions",
			body:     []byte("not json"),
			provider: ProviderConfig{Model: "gpt-4.1"},
			expected: RequestMeta{
				EffectiveBody: []byte("not json"),
			},
		},
		{
			name:     "provider model override builds cache key",
			path:     "/v1/chat/completions",
			body:     []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`),
			provider: ProviderConfig{Model: "gpt-4.1"},
			expected: RequestMeta{
				EffectiveBody:  []byte(`{"messages":[{"content":"hi","role":"user"}],"model":"gpt-4.1"}`),
				EffectiveModel: "gpt-4.1",
			},
		},
		{
			name: "stream adds default stream options",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"gpt-4o","messages":[],"stream":true}`),
			expected: RequestMeta{
				EffectiveBody:  []byte(`{"messages":[],"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true}}`),
				EffectiveModel: "gpt-4o",
				Stream:         true,
			},
		},
		{
			name: "stream preserves user stream options",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"gpt-4o","messages":[],"stream":true,"stream_options":{"include_usage":false}}`),
			expected: RequestMeta{
				EffectiveBody:  []byte(`{"messages":[],"model":"gpt-4o","stream":true,"stream_options":{"include_usage":false}}`),
				EffectiveModel: "gpt-4o",
				Stream:         true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := AnalyzeRequest(tt.path, tt.body, tt.provider)
			if err != nil {
				t.Fatalf("AnalyzeRequest() error = %v", err)
			}

			expected := tt.expected
			if expected.CacheKey == "" && len(expected.EffectiveBody) > 0 {
				payload, ok := decodeJSONObject(expected.EffectiveBody)
				if ok {
					cacheKey, err := buildCacheKey(tt.path, payload)
					if err != nil {
						t.Fatalf("buildCacheKey() error = %v", err)
					}
					expected.CacheKey = cacheKey
				}
			}

			meta.EffectiveBody = canonicalBody(t, meta.EffectiveBody)
			expected.EffectiveBody = canonicalBody(t, expected.EffectiveBody)

			if !reflect.DeepEqual(meta, expected) {
				t.Fatalf("AnalyzeRequest() = %#v, want %#v", meta, expected)
			}
		})
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
