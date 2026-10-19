package aimenshen

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestServeAPIHTTPSingleProviderNetworkErrorKeepsUpstreamCause(t *testing.T) {
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
			"dead": {Name: "dead", BaseURL: deadURL, Weight: 1},
		},
		Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status code = %d, want 502 Bad Gateway", rec.Code)
	}
	if !strings.Contains(logBuf.String(), "send upstream request") {
		t.Fatalf("log output missing upstream cause:\n%s", logBuf.String())
	}

	// saveExchange is asynchronous; wait for the worker to flush.
	var detail *LogDetail
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := storage.RequestLogs(1, 10)
		if err != nil {
			t.Fatalf("RequestLogs() error = %v", err)
		}
		if len(logs) > 0 {
			detail, err = storage.RequestDetail(logs[0].ID)
			if err != nil {
				t.Fatalf("RequestDetail() error = %v", err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if detail == nil {
		t.Fatal("stored exchange not found")
	}
	if !strings.Contains(detail.ResponseBody, "send upstream request") {
		t.Errorf("stored response body = %q, want upstream error cause", detail.ResponseBody)
	}

	var errPayload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(detail.ResponseBody), &errPayload); err != nil {
		t.Fatalf("unmarshal error body %q: %v", detail.ResponseBody, err)
	}
	if !strings.Contains(errPayload.Error.Message, "send upstream request") {
		t.Errorf("error message = %q, want upstream error cause", errPayload.Error.Message)
	}
}

func TestServeAPIHTTPFailoverKeepsMetadataOfReturnedResponse(t *testing.T) {
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
			"primary":   {Name: "primary", BaseURL: deadURL, Weight: 1000},
			"secondary": {Name: "secondary", BaseURL: server500.URL, Weight: 50},
			"tertiary":  {Name: "tertiary", BaseURL: deadURL, Weight: 1},
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

	// Since primary (weight 1000) was picked first, failover tries secondary (500), then tertiary (network error).
	// The returned response should be secondary's 500.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}

	var detail *LogDetail
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := storage.RequestLogs(1, 10)
		if err != nil {
			t.Fatalf("RequestLogs() error = %v", err)
		}
		if len(logs) > 0 {
			detail, err = storage.RequestDetail(logs[0].ID)
			if err != nil {
				t.Fatalf("RequestDetail() error = %v", err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if detail == nil {
		t.Fatal("stored exchange not found")
	}
	if detail.Provider != "secondary" {
		t.Errorf("stored provider = %q, want secondary", detail.Provider)
	}
	if detail.StatusCode != http.StatusInternalServerError {
		t.Errorf("stored status code = %d, want 500", detail.StatusCode)
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

func TestProviderUsageReports(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "report_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	p100 := int64(100)
	c50 := int64(50)
	t150 := int64(150)
	ca20 := int64(20)

	// Insert exchange for provider openai
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-1", CreatedAt: time.Now(), Model: "gpt-4o", Provider: "openai", Path: "/chat/completions"},
		ResponseLog{RequestID: "req-1", StatusCode: 200, DurationMS: 120, FromCache: false},
		&UsageLog{RequestID: "req-1", PromptTokens: &p100, CompletionTokens: &c50, TotalTokens: &t150, CachedTokens: &ca20},
	)

	// Insert cached exchange for provider openai
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-2", CreatedAt: time.Now(), Model: "gpt-4o", Provider: "openai", Path: "/chat/completions"},
		ResponseLog{RequestID: "req-2", StatusCode: 200, DurationMS: 5, FromCache: true},
		nil,
	)

	// Insert exchange for provider deepseek
	p200 := int64(200)
	c100 := int64(100)
	t300 := int64(300)
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-3", CreatedAt: time.Now(), Model: "deepseek-chat", Provider: "deepseek", Path: "/chat/completions"},
		ResponseLog{RequestID: "req-3", StatusCode: 200, DurationMS: 200, FromCache: false},
		&UsageLog{RequestID: "req-3", PromptTokens: &p200, CompletionTokens: &c100, TotalTokens: &t300},
	)

	reports, err := storage.ProviderUsageReports(7)
	if err != nil {
		t.Fatalf("ProviderUsageReports() error = %v", err)
	}

	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2", len(reports))
	}

	byProvider := map[string]ProviderUsageReport{}
	for _, r := range reports {
		byProvider[r.Provider] = r
	}

	deepseek, ok := byProvider["deepseek"]
	if !ok || deepseek.TotalTokens != 300 || deepseek.RequestCount != 1 {
		t.Fatalf("deepseek report = %+v", deepseek)
	}

	openai, ok := byProvider["openai"]
	if !ok || openai.TotalTokens != 150 || openai.RequestCount != 2 || openai.CacheHits != 1 || openai.CachedTokens != 20 {
		t.Fatalf("openai report = %+v", openai)
	}

	// Test Gateway HTTP endpoint /__report/providers
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"openai": {Name: "openai", BaseURL: "https://api.openai.com", Weight: 1},
		},
		Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}
	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/__report/providers?days=7", nil)
	gateway.ServeWebHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var endpointReports []ProviderUsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &endpointReports); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	if len(endpointReports) != 2 {
		t.Fatalf("endpoint len(reports) = %d, want 2", len(endpointReports))
	}
}

func TestResolveProvider(t *testing.T) {
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"primary":  {Name: "primary", BaseURL: "https://primary.example", Weight: 10},
			"disabled": {Name: "disabled", BaseURL: "https://disabled.example", Weight: 0},
		},
	}
	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	// 1. Unspecified: uses pickProvider and leaves the path alone
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	p, path, explicit, err := gateway.resolveProvider(req.URL.Path)
	if err != nil || explicit || p.Name != "primary" || path != "/chat/completions" {
		t.Fatalf("resolveProvider() = (%+v, %q, %v, %v), want primary, explicit=false, path=/chat/completions", p, path, explicit, err)
	}

	// 2. Explicit active provider via /provider/ prefix
	req = httptest.NewRequest(http.MethodPost, "/provider/primary/chat/completions", nil)
	p, path, explicit, err = gateway.resolveProvider(req.URL.Path)
	if err != nil || !explicit || p.Name != "primary" || path != "/chat/completions" {
		t.Fatalf("resolveProvider() = (%+v, %q, %v, %v), want primary, explicit=true, path=/chat/completions", p, path, explicit, err)
	}

	// 3. Explicit zero-weight provider via /provider/ prefix
	req = httptest.NewRequest(http.MethodPost, "/provider/disabled/chat/completions", nil)
	p, path, explicit, err = gateway.resolveProvider(req.URL.Path)
	if err != nil || !explicit || p.Name != "disabled" || path != "/chat/completions" {
		t.Fatalf("resolveProvider() = (%+v, %q, %v, %v), want disabled, explicit=true, path=/chat/completions", p, path, explicit, err)
	}

	// 4. Non-existent provider via /provider/ prefix
	req = httptest.NewRequest(http.MethodPost, "/provider/nonexistent/chat/completions", nil)
	_, _, _, err = gateway.resolveProvider(req.URL.Path)
	if err == nil || !strings.Contains(err.Error(), `"nonexistent"`) {
		t.Fatalf("resolveProvider() error = %v, want unknown provider error", err)
	}

	// 5. Bare /provider/<name> without trailing path targets the upstream root
	req = httptest.NewRequest(http.MethodGet, "/provider/primary", nil)
	p, path, explicit, err = gateway.resolveProvider(req.URL.Path)
	if err != nil || !explicit || p.Name != "primary" || path != "/" {
		t.Fatalf("resolveProvider() = (%+v, %q, %v, %v), want primary, explicit=true, path=/", p, path, explicit, err)
	}

	// 6. Trailing slash is preserved so nested upstream paths survive
	req = httptest.NewRequest(http.MethodPost, "/provider/primary/v1/chat/completions", nil)
	p, path, explicit, err = gateway.resolveProvider(req.URL.Path)
	if err != nil || !explicit || p.Name != "primary" || path != "/v1/chat/completions" {
		t.Fatalf("resolveProvider() = (%+v, %q, %v, %v), want primary, explicit=true, path=/v1/chat/completions", p, path, explicit, err)
	}

	// 7. Missing provider name is rejected instead of falling back to random routing
	for _, badPath := range []string{"/provider", "/provider/", "/provider//chat/completions"} {
		_, _, _, err = gateway.resolveProvider(badPath)
		if err == nil || !strings.Contains(err.Error(), "missing provider name") {
			t.Fatalf("resolveProvider(%q) error = %v, want missing provider name", badPath, err)
		}
	}

	// 8. resolveProvider does not touch the request
	req = httptest.NewRequest(http.MethodPost, "/provider/primary/chat/completions", nil)
	if _, _, _, err := gateway.resolveProvider(req.URL.Path); err != nil {
		t.Fatalf("resolveProvider() error = %v", err)
	}
	if req.URL.Path != "/provider/primary/chat/completions" {
		t.Fatalf("req.URL.Path = %q, want the request to be left untouched", req.URL.Path)
	}
}

func TestServeAPIHTTPWithProviderPathPrefix(t *testing.T) {
	var receivedByA, receivedByB bool
	var querySeenByB, pathSeenByB string

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedByA = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"response A"}}]}`)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedByB = true
		pathSeenByB = r.URL.Path
		querySeenByB = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"response B"}}]}`)
	}))
	defer serverB.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"provA": {Name: "provA", BaseURL: serverA.URL, Weight: 100},
			"provB": {Name: "provB", BaseURL: serverB.URL, Weight: 1},
		},
		Failover: FailoverConfig{Enable: true},
		Storage:  StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}
	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	// 1. Explicitly request provB via /provider/provB path prefix
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/provider/provB/chat/completions?custom=yes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	if receivedByA || !receivedByB {
		t.Fatalf("expected request routed to provB only (A=%v, B=%v)", receivedByA, receivedByB)
	}
	if pathSeenByB != "/chat/completions" {
		t.Fatalf("pathSeenByB = %q, want '/chat/completions' with /provider/provB stripped", pathSeenByB)
	}
	if querySeenByB != "custom=yes" {
		t.Fatalf("querySeenByB = %q, want 'custom=yes' preserved", querySeenByB)
	}

	// 2. Request nonexistent provider returns 400
	req = httptest.NewRequest(http.MethodPost, "/provider/invalid/chat/completions", strings.NewReader(body))
	rec = httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400 Bad Request", rec.Code)
	}

	// 3. Missing provider name returns 400 instead of routing by weight
	receivedByA, receivedByB = false, false
	for _, missingNamePath := range []string{"/provider", "/provider/", "/provider//chat/completions"} {
		req = httptest.NewRequest(http.MethodPost, missingNamePath, strings.NewReader(body))
		rec = httptest.NewRecorder()
		gateway.ServeAPIHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status code for %q = %d, want 400 Bad Request", missingNamePath, rec.Code)
		}
	}
	if receivedByA || receivedByB {
		t.Fatalf("missing provider name must not reach a provider (A=%v, B=%v)", receivedByA, receivedByB)
	}

	// 4. Explicit provider does not failover when it errors
	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provErr 500 error", http.StatusInternalServerError)
	}))
	defer serverErr.Close()

	cfg.Providers["provErr"] = ProviderConfig{Name: "provErr", BaseURL: serverErr.URL, Weight: 1}
	gatewayWithErr, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	receivedByA = false
	req = httptest.NewRequest(http.MethodPost, "/provider/provErr/chat/completions", strings.NewReader(body))
	rec = httptest.NewRecorder()
	gatewayWithErr.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500 from provErr without failover", rec.Code)
	}
	if receivedByA {
		t.Fatal("should not failover to provA when provider was explicitly requested")
	}
}

func TestProxyPassthroughWithProviderPathPrefix(t *testing.T) {
	var requestedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"upstream": {Name: "upstream", BaseURL: server.URL, Weight: 1},
		},
	}
	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/provider/upstream/models?extra=1", nil)
	rec := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	if requestedURL != "/models?extra=1" {
		t.Fatalf("upstream received URL = %q, want '/models?extra=1'", requestedURL)
	}
}

func TestProxyStreamClientCanceledLogs499(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: chunk1\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"upstream": {Name: "upstream", BaseURL: server.URL, Weight: 1},
		},
		Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}
	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(reqBody)).WithContext(ctx)

	// Custom response writer that cancels context after first chunk
	cancelAfterFirst := &cancelingResponseWriter{
		ResponseWriter: httptest.NewRecorder(),
		cancel:         cancel,
	}

	gateway.ServeAPIHTTP(cancelAfterFirst, req)

	// Wait briefly for async log saving to settle
	time.Sleep(50 * time.Millisecond)

	logs, err := storage.RequestLogs(1, 10)
	if err != nil {
		t.Fatalf("RequestLogs() error = %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected request log to be saved")
	}
	if logs[0].StatusCode != 499 {
		t.Fatalf("status code in storage = %d, want 499 (Client Closed Request)", logs[0].StatusCode)
	}
}

type cancelingResponseWriter struct {
	http.ResponseWriter
	cancel context.CancelFunc
	called bool
}

func (c *cancelingResponseWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	if !c.called {
		c.called = true
		c.cancel() // cancel client context right after first write
	}
	return n, err
}

func (c *cancelingResponseWriter) Flush() {
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func TestServeWebPages(t *testing.T) {
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"primary": {Name: "primary", BaseURL: "https://primary.example", Weight: 10},
		},
	}
	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	tests := []struct {
		path         string
		expectedCode int
		contains     string
	}{
		{"/", http.StatusOK, "Overview"},
		{"/overview", http.StatusOK, "Requests Trend"},
		{"/logs", http.StatusOK, "Audit Logs"},
		{"/logs/req-123456", http.StatusOK, "Audit Logs"},
		{"/assets/pico.min.css", http.StatusOK, "Pico CSS"},
		{"/assets/app.css", http.StatusOK, "--nav-active-bg"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			gateway.ServeWebHTTP(rec, req)

			if rec.Code != tt.expectedCode {
				t.Errorf("%s status code = %d, want %d", tt.path, rec.Code, tt.expectedCode)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.contains) {
				t.Errorf("%s body does not contain %q", tt.path, tt.contains)
			}
		})
	}
}

func TestRequestLogsMessagesCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_msg_counts.db")
	storage, err := OpenStorage(StorageConfig{
		SQLite: SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	// 1. Chat completion with 4 messages
	reqBody1 := []byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`)
	respBody1 := []byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-multi", CreatedAt: time.Now(), Model: "gpt-4o", Path: "/chat/completions", RequestBody: reqBody1},
		ResponseLog{RequestID: "req-multi", StatusCode: 200, ResponseBody: respBody1},
		nil,
	)

	// 2. Error response
	reqBody2 := []byte(`{"messages":[{"role":"user","content":"error test"}]}`)
	respBody2 := []byte(`{"error":{"message":"bad request"}}`)
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-err", CreatedAt: time.Now(), Model: "gpt-4o", Path: "/chat/completions", RequestBody: reqBody2},
		ResponseLog{RequestID: "req-err", StatusCode: 400, ResponseBody: respBody2},
		nil,
	)

	// 3. Streaming response (SSE)
	reqBody3 := []byte(`{"messages":[{"role":"user","content":"stream test"}]}`)
	respBody3 := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-stream", CreatedAt: time.Now(), Model: "gpt-4o", Path: "/chat/completions", RequestBody: reqBody3},
		ResponseLog{RequestID: "req-stream", StatusCode: 200, ResponseBody: respBody3},
		nil,
	)

	logs, err := storage.RequestLogs(1, 10)
	if err != nil {
		t.Fatalf("RequestLogs() error = %v", err)
	}

	byID := make(map[string]LogEntry)
	for _, l := range logs {
		byID[l.ID] = l
	}

	// Verify multi-turn request
	l1, ok := byID["req-multi"]
	if !ok {
		t.Fatal("req-multi not found")
	}
	if l1.MessagesCount != 4 {
		t.Errorf("req-multi MessagesCount = %d, want 4", l1.MessagesCount)
	}
	if l1.ResponseMessages != 1 {
		t.Errorf("req-multi ResponseMessages = %d, want 1", l1.ResponseMessages)
	}
	if l1.RequestBodyPreview != "s" {
		t.Errorf("req-multi RequestBodyPreview = %q, want %q", l1.RequestBodyPreview, "s")
	}
	if l1.ResponseBodyPreview != "hello" {
		t.Errorf("req-multi ResponseBodyPreview = %q, want %q", l1.ResponseBodyPreview, "hello")
	}

	// Verify error response
	l2, ok := byID["req-err"]
	if !ok {
		t.Fatal("req-err not found")
	}
	if l2.MessagesCount != 1 {
		t.Errorf("req-err MessagesCount = %d, want 1", l2.MessagesCount)
	}
	if l2.ResponseMessages != 0 {
		t.Errorf("req-err ResponseMessages = %d, want 0", l2.ResponseMessages)
	}
	if l2.RequestBodyPreview != "error test" {
		t.Errorf("req-err RequestBodyPreview = %q, want %q", l2.RequestBodyPreview, "error test")
	}
	if l2.ResponseBodyPreview != "bad request" {
		t.Errorf("req-err ResponseBodyPreview = %q, want %q", l2.ResponseBodyPreview, "bad request")
	}

	// Verify streaming response
	l3, ok := byID["req-stream"]
	if !ok {
		t.Fatal("req-stream not found")
	}
	if l3.MessagesCount != 1 {
		t.Errorf("req-stream MessagesCount = %d, want 1", l3.MessagesCount)
	}
	if l3.ResponseMessages != 1 {
		t.Errorf("req-stream ResponseMessages = %d, want 1", l3.ResponseMessages)
	}
}

func TestStorageDailyUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_daily_usage.db")
	storage, err := OpenStorage(StorageConfig{
		SQLite: SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	// Insert direct request
	req1 := RequestLog{
		ID:        "req-direct",
		CreatedAt: time.Now(),
		Model:     "gpt-4o",
	}
	resp1 := ResponseLog{
		RequestID:  "req-direct",
		StatusCode: 200,
		FromCache:  false,
	}
	p10 := int64(10)
	c20 := int64(20)
	t30 := int64(30)
	usage1 := &UsageLog{
		RequestID:        "req-direct",
		PromptTokens:     &p10,
		CompletionTokens: &c20,
		TotalTokens:      &t30,
	}
	if err := storage.saveExchangeSync(req1, resp1, usage1); err != nil {
		t.Fatalf("saveExchangeSync() error = %v", err)
	}

	// Insert cache hit request
	req2 := RequestLog{
		ID:        "req-cached",
		CreatedAt: time.Now(),
		Model:     "gpt-4o",
	}
	resp2 := ResponseLog{
		RequestID:  "req-cached",
		StatusCode: 200,
		FromCache:  true,
	}
	ca30 := int64(30)
	usage2 := &UsageLog{
		RequestID:    "req-cached",
		CachedTokens: &ca30,
		TotalTokens:  &t30,
	}
	if err := storage.saveExchangeSync(req2, resp2, usage2); err != nil {
		t.Fatalf("saveExchangeSync() error = %v", err)
	}

	daily, err := storage.DailyUsage(7)
	if err != nil {
		t.Fatalf("DailyUsage() error = %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("len(daily) = %d, want 1", len(daily))
	}

	d := daily[0]
	if d.RequestCount != 2 {
		t.Errorf("d.RequestCount = %d, want 2", d.RequestCount)
	}
	if d.CacheHits != 1 {
		t.Errorf("d.CacheHits = %d, want 1", d.CacheHits)
	}
	if d.PromptTokens != 10 {
		t.Errorf("d.PromptTokens = %d, want 10", d.PromptTokens)
	}
	if d.CompletionTokens != 20 {
		t.Errorf("d.CompletionTokens = %d, want 20", d.CompletionTokens)
	}
}

func TestServeAPIHTTPFailoverShortCircuitsOnClientContextCanceled(t *testing.T) {
	var cancel context.CancelFunc
	serverPrimary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cancel client context upon primary receiving the request, simulating client disconnect
		if cancel != nil {
			cancel()
		}
		http.Error(w, "primary failed", http.StatusInternalServerError)
	}))
	defer serverPrimary.Close()

	var secondaryHits int32
	serverSecondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondaryHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverSecondary.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"primary":   {Name: "primary", BaseURL: serverPrimary.URL, Weight: 1000},
			"secondary": {Name: "secondary", BaseURL: serverSecondary.URL, Weight: 1},
		},
		Failover: FailoverConfig{Enable: true},
		Storage:  StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	cancel = cancelFn

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(reqBody)).WithContext(ctx)
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)

	if atomic.LoadInt32(&secondaryHits) > 0 {
		t.Errorf("secondary provider was hit %d times, want 0 when client context is canceled", secondaryHits)
	}
}
