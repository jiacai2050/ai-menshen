package aimenshen

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPickProviderSkipsZeroWeightProviders(t *testing.T) {
	gateway, err := NewGateway(Config{
		Providers: []ProviderConfig{
			{BaseURL: "https://disabled.example", Weight: 0},
			{BaseURL: "https://active.example", Weight: 2},
		},
		Upstream: UpstreamConfig{Timeout: 1},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	provider := gateway.pickProvider()
	if provider.BaseURL != "https://active.example" {
		t.Fatalf("pickProvider() = %q, want active provider", provider.BaseURL)
	}
}

func TestNewGatewayPrecomputesActiveProviders(t *testing.T) {
	gateway, err := NewGateway(Config{
		Providers: []ProviderConfig{
			{BaseURL: "https://disabled.example", Weight: 0},
			{BaseURL: "https://active-a.example", Weight: 2},
			{BaseURL: "https://active-b.example", Weight: 3},
		},
		Upstream: UpstreamConfig{Timeout: 1},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	if got := len(gateway.activeProviders); got != 2 {
		t.Fatalf("len(activeProviders) = %d, want 2", got)
	}
	if got := gateway.activeTotalWeight; got != 5 {
		t.Fatalf("activeTotalWeight = %d, want 5", got)
	}
}

func TestForwardUpstreamUsesSelectedProvider(t *testing.T) {
	var authHeader string
	var customHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		customHeader = r.Header.Get("X-Upstream-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	gateway := &Gateway{
		client: server.Client(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	resp, _, err := gateway.forwardUpstream(req, http.NoBody, ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-upstream",
		Headers: map[string]string{"X-Upstream-Key": "provider"},
	})
	if err != nil {
		t.Fatalf("forwardUpstream() error = %v", err)
	}
	defer resp.Body.Close()

	if authHeader != "Bearer sk-upstream" {
		t.Fatalf("Authorization header = %q, want provider API key", authHeader)
	}
	if customHeader != "provider" {
		t.Fatalf("X-Upstream-Key header = %q, want provider header", customHeader)
	}
}
