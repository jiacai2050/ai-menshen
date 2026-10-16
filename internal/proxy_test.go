package aimenshen

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestPickProviderSkipsZeroWeightProviders(t *testing.T) {
	gateway, err := NewGateway(Config{
		Providers: map[string]ProviderConfig{
			"disabled": {Name: "disabled", BaseURL: "https://disabled.example", Weight: 0},
			"active":   {Name: "active", BaseURL: "https://active.example", Weight: 2},
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
		Providers: map[string]ProviderConfig{
			"disabled": {Name: "disabled", BaseURL: "https://disabled.example", Weight: 0},
			"a":        {Name: "a", BaseURL: "https://active-a.example", Weight: 2},
			"b":        {Name: "b", BaseURL: "https://active-b.example", Weight: 3},
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

func TestBuildActiveProvidersOrdersForFailover(t *testing.T) {
	providers := map[string]ProviderConfig{
		"light":    {Name: "light", BaseURL: "https://light.example", Weight: 1},
		"heavy-b":  {Name: "heavy-b", BaseURL: "https://heavy-b.example", Weight: 3},
		"heavy-a":  {Name: "heavy-a", BaseURL: "https://heavy-a.example", Weight: 3},
		"disabled": {Name: "disabled", BaseURL: "https://disabled.example", Weight: 0},
	}

	active, total := buildActiveProviders(providers)
	if total != 7 {
		t.Fatalf("totalWeight = %d, want 7", total)
	}

	var names []string
	for _, provider := range active {
		names = append(names, provider.Name)
	}
	want := []string{"heavy-a", "heavy-b", "light"}
	if len(names) != len(want) {
		t.Fatalf("activeProviders names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("activeProviders names = %v, want %v", names, want)
		}
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

	provider := ProviderConfig{
		Name:    "upstream",
		BaseURL: server.URL,
		APIKey:  "sk-upstream",
		Headers: map[string]string{"X-Upstream-Key": "provider"},
	}
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	gateway := &Gateway{
		clients: map[string]*http.Client{
			"": server.Client(),
		},
		bases: map[string]*url.URL{server.URL: base},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	resp, _, err := gateway.forwardUpstream(req, http.NoBody, provider)
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

func TestServeAPIHTTPFailoverAllNetworkErrorsReturns502(t *testing.T) {
	// Both servers are closed immediately to produce network connection errors
	serverDead1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL1 := serverDead1.URL
	serverDead1.Close()

	serverDead2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL2 := serverDead2.URL
	serverDead2.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"dead1": {Name: "dead1", BaseURL: deadURL1, Weight: 1},
			"dead2": {Name: "dead2", BaseURL: deadURL2, Weight: 1},
		},
		Failover: FailoverConfig{Enable: true},
		Storage:  StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status code = %d, want 502 Bad Gateway", rec.Code)
	}
}

func TestServeAPIHTTPFailover500FollowedByNetworkErrorDoesNotPanic(t *testing.T) {
	// One server returns 500, other server produces network error.
	server500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream failed", http.StatusInternalServerError)
	}))
	defer server500.Close()

	serverDead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := serverDead.URL
	serverDead.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"primary": {Name: "primary", BaseURL: server500.URL, Weight: 5},
			"dead":    {Name: "dead", BaseURL: deadURL, Weight: 5},
		},
		Failover: FailoverConfig{Enable: true},
		Storage:  StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	// Send multiple requests to ensure both picking orders are tested without panicking.
	for range 10 {
		reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(reqBody))
		rec := httptest.NewRecorder()

		gateway.ServeAPIHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusBadGateway {
			t.Fatalf("status code = %d, want 500 or 502", rec.Code)
		}
	}
}
