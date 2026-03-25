package aimenshen

import (
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
			expectKey: "33a9d5599056bf6d61f3825366ec5924db8ee25bbab8c3fabcbd869c22f52f9b",
		},
		{
			name:      "same payload, different path",
			path:      "/v1/completions",
			payload:   `{"model":"gpt-4o","temp":0.7}`,
			expectKey: "a02ac362ad0e02dcfd460c778028aad15fa1f58dc99506cc0534e90c81ec7861",
		},
		{
			name:      "different key order",
			path:      "/v1/chat/completions",
			payload:   `{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o"}`,
			expectKey: "6a0e24eb3d08fc248852799017862dc6e19e0b323e3417735c63c90c45384762",
		},
		{
			name:      "different key order (reversed)",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			expectKey: "6a0e24eb3d08fc248852799017862dc6e19e0b323e3417735c63c90c45384762",
		},
		{
			name:      "excluded fields (user)",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","messages":[],"user":"user-123"}`,
			expectKey: "767d478c9349beda7e10ee0590b86fcaf57bc7ff48bd5c4229908834923e144f",
		},
		{
			name:      "excluded fields (stream_options)",
			path:      "/v1/chat/completions",
			payload:   `{"model":"gpt-4o","messages":[],"stream_options":{"include_usage":true}}`,
			expectKey: "767d478c9349beda7e10ee0590b86fcaf57bc7ff48bd5c4229908834923e144f",
		},
		{
			name:      "float consistency (verify 1)",
			path:      "/v1/chat/completions",
			payload:   `{"temp": 0.0000001, "top_p": 1}`,
			expectKey: "156c816deb0adc2b84702ceffa009ce5b9ec83dd366b356147ad3df74f6f96ac",
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
