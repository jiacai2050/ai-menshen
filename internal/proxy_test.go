package menshen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestPickProviderForCapabilityWeightDistribution(t *testing.T) {
	gateway, err := NewGateway(Config{
		Providers: map[string]ProviderConfig{
			"heavy": {
				Name:         "heavy",
				BaseURL:      "https://heavy.example",
				Weight:       3,
				Capabilities: []string{CapOpenAICompletions},
			},
			"light": {
				Name:         "light",
				BaseURL:      "https://light.example",
				Weight:       1,
				Capabilities: []string{CapOpenAICompletions},
			},
			"other": {
				Name:         "other",
				BaseURL:      "https://other.example",
				Weight:       10,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
		Upstream: UpstreamConfig{Timeout: 1},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	counts := make(map[string]int)
	const iterations = 2000
	for i := 0; i < iterations; i++ {
		p, err := gateway.pickProviderForCapability(CapOpenAICompletions)
		if err != nil {
			t.Fatalf("pickProviderForCapability failed: %v", err)
		}
		counts[p.Name]++
	}

	if counts["other"] != 0 {
		t.Errorf("provider 'other' was picked %d times, want 0", counts["other"])
	}
	// heavy should be ~75% (1500), light should be ~25% (500). Allow safe margin [1200, 1800].
	if counts["heavy"] < 1200 || counts["heavy"] > 1800 {
		t.Errorf("heavy picked %d times, expected roughly ~1500 (75%%)", counts["heavy"])
	}
	if counts["light"] < 200 || counts["light"] > 800 {
		t.Errorf("light picked %d times, expected roughly ~500 (25%%)", counts["light"])
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
	logs := waitForRequestLogs(t, storage, 1)
	detail, err := storage.RequestDetail(logs[0].ID)
	if err != nil {
		t.Fatalf("RequestDetail() error = %v", err)
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

	logs := waitForRequestLogs(t, storage, 1)
	detail, err := storage.RequestDetail(logs[0].ID)
	if err != nil {
		t.Fatalf("RequestDetail() error = %v", err)
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

	// 1. Unspecified: leaves path alone and explicit=false (deferred to capability-based picking)
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	p, path, explicit, err := gateway.resolveProvider(req.URL.Path)
	if err != nil || explicit || path != "/chat/completions" {
		t.Fatalf("resolveProvider() = (%+v, %q, %v, %v), want explicit=false, path=/chat/completions", p, path, explicit, err)
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

	// Wait for worker goroutine to process exchange
	logs := waitForRequestLogs(t, storage, 1)
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

	// 4. Anthropic request with system prompt + messages, and response with thinking block + text block
	reqBody4 := []byte(`{"system":"you are helpful","messages":[{"role":"user","content":[{"type":"text","text":"hello claude"}]}]}`)
	respBody4 := []byte(`{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"let me think"},{"type":"text","text":"hi human"}]}`)
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-anthropic", CreatedAt: time.Now(), Model: "claude-3-5-sonnet", Path: "/v1/messages", RequestBody: reqBody4},
		ResponseLog{RequestID: "req-anthropic", StatusCode: 200, ResponseBody: respBody4},
		nil,
	)

	// 5. Anthropic response where content[0].text is empty string, followed by content[1].text with real content
	reqBody5 := []byte(`{"system":"","messages":[{"role":"user","content":"hello"}]}`) // empty system should not increment count
	respBody5 := []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"second block text"}]}`)
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-empty-fallback", CreatedAt: time.Now(), Model: "claude-3-5-sonnet", Path: "/v1/messages", RequestBody: reqBody5},
		ResponseLog{RequestID: "req-empty-fallback", StatusCode: 200, ResponseBody: respBody5},
		nil,
	)

	// 6. Request with system only (no messages array)
	reqBody6 := []byte(`{"system":"system only instruction"}`)
	respBody6 := []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`)
	_ = storage.saveExchangeSync(
		RequestLog{ID: "req-system-only", CreatedAt: time.Now(), Model: "claude-3-5-sonnet", Path: "/v1/messages", RequestBody: reqBody6},
		ResponseLog{RequestID: "req-system-only", StatusCode: 200, ResponseBody: respBody6},
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

	// Verify Anthropic request & response
	l4, ok := byID["req-anthropic"]
	if !ok {
		t.Fatal("req-anthropic not found")
	}
	if l4.MessagesCount != 2 { // 1 system + 1 user message
		t.Errorf("req-anthropic MessagesCount = %d, want 2", l4.MessagesCount)
	}
	if l4.ResponseMessages != 1 {
		t.Errorf("req-anthropic ResponseMessages = %d, want 1", l4.ResponseMessages)
	}
	if l4.RequestBodyPreview != "you are helpful" {
		t.Errorf("req-anthropic RequestBodyPreview = %q, want %q", l4.RequestBodyPreview, "you are helpful")
	}
	if l4.ResponseBodyPreview != "hi human" {
		t.Errorf("req-anthropic ResponseBodyPreview = %q, want %q", l4.ResponseBodyPreview, "hi human")
	}

	// Verify empty string fallback: content[0].text is empty, should extract content[1].text
	l5, ok := byID["req-empty-fallback"]
	if !ok {
		t.Fatal("req-empty-fallback not found")
	}
	if l5.MessagesCount != 1 { // system is "" so count should be 1 (only user message)
		t.Errorf("req-empty-fallback MessagesCount = %d, want 1", l5.MessagesCount)
	}
	if l5.ResponseBodyPreview != "second block text" {
		t.Errorf("req-empty-fallback ResponseBodyPreview = %q, want %q", l5.ResponseBodyPreview, "second block text")
	}

	// Verify system-only request
	l6, ok := byID["req-system-only"]
	if !ok {
		t.Fatal("req-system-only not found")
	}
	if l6.MessagesCount != 1 {
		t.Errorf("req-system-only MessagesCount = %d, want 1", l6.MessagesCount)
	}
	if l6.RequestBodyPreview != "system only instruction" {
		t.Errorf("req-system-only RequestBodyPreview = %q, want %q", l6.RequestBodyPreview, "system only instruction")
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

func TestV1ChatCompletionsAuditingAndRouting(t *testing.T) {
	var openaiHits int32
	serverOpenAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&openaiHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-v1",
			"object": "chat.completion",
			"created": 1677652288,
			"model": "gpt-4o",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}}],
			"usage": {
				"prompt_tokens": 15,
				"completion_tokens": 25,
				"total_tokens": 40
			}
		}`))
	}))
	defer serverOpenAI.Close()

	var claudeHits int32
	serverClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&claudeHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverClaude.Close()

	dbPath := filepath.Join(t.TempDir(), "v1_chat_audit_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"openai": {
				Name:         "openai",
				BaseURL:      serverOpenAI.URL,
				Weight:       1,
				Capabilities: []string{CapOpenAICompletions},
			},
			"claude": {
				Name:         "claude",
				BaseURL:      serverClaude.URL,
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
		Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway() error = %v", err)
	}

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&openaiHits) != 1 {
		t.Errorf("openai hits = %d, want 1", openaiHits)
	}
	if atomic.LoadInt32(&claudeHits) != 0 {
		t.Errorf("claude hits = %d, want 0", claudeHits)
	}

	// Verify asynchronous audit persistence
	var summary *UsageSummary
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, err := storage.UsageSummary(1)
		if err == nil && s != nil && s.TotalTokens > 0 {
			summary = s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if summary == nil {
		t.Fatal("expected usage summary persisted for /v1/chat/completions")
	}
	if summary.PromptTokens != 15 {
		t.Errorf("prompt tokens = %d, want 15", summary.PromptTokens)
	}
	if summary.CompletionTokens != 25 {
		t.Errorf("completion tokens = %d, want 25", summary.CompletionTokens)
	}
	if summary.TotalTokens != 40 {
		t.Errorf("total tokens = %d, want 40", summary.TotalTokens)
	}

	logs, err := storage.RequestLogs(1, 10)
	if err != nil || len(logs) == 0 {
		t.Fatalf("expected request logs, got err=%v len=%d", err, len(logs))
	}
	if logs[0].Path != "/v1/chat/completions" {
		t.Errorf("logged path = %q, want /v1/chat/completions", logs[0].Path)
	}
}

func TestGatewayAuthTokenNotLeakedUpstream(t *testing.T) {
	var capturedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	cfg := Config{
		Auth: AuthConfig{
			Enable: true,
			Token:  "gateway-secret-token",
		},
		Providers: map[string]ProviderConfig{
			"unauthenticated": {
				Name:    "unauthenticated",
				BaseURL: server.URL,
				APIKey:  "", // Provider has no API key configured
				Weight:  1,
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	// 1. Auditable path (/chat/completions)
	req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req1.Header.Set("Authorization", "Bearer gateway-secret-token")
	rec1 := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 failed: %d", rec1.Code)
	}
	if capturedAuth != "" {
		t.Errorf("capturedAuth = %q, want empty (gateway auth token must not leak upstream)", capturedAuth)
	}

	// 2. Passthrough path (/models)
	req2 := httptest.NewRequest(http.MethodGet, "/models", nil)
	req2.Header.Set("Authorization", "Bearer gateway-secret-token")
	rec2 := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 failed: %d", rec2.Code)
	}
	if capturedAuth != "" {
		t.Errorf("capturedAuth = %q, want empty (gateway auth token must not leak upstream in passthrough)", capturedAuth)
	}
}

func TestTransparentOpenAIProxyWhenNoAPIKey(t *testing.T) {
	var capturedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"direct-openai": {
				Name:         "direct-openai",
				BaseURL:      server.URL,
				APIKey:       "", // BYOK: client supplies their own key
				Weight:       1,
				Capabilities: []string{CapOpenAICompletions},
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer client-custom-openai-key")
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request failed: %d", rec.Code)
	}
	if capturedAuth != "Bearer client-custom-openai-key" {
		t.Errorf("capturedAuth = %q, want 'Bearer client-custom-openai-key'", capturedAuth)
	}
}

func TestGatewayAuthWithAnthropicApiKeyHeader(t *testing.T) {
	var capturedAuth string
	var capturedApiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedApiKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		Auth: AuthConfig{
			Enable: true,
			Token:  "gateway-secret-token",
		},
		Providers: map[string]ProviderConfig{
			"claude": {
				Name:         "claude",
				BaseURL:      server.URL,
				APIKey:       "sk-ant-real-upstream-key",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	// 1. Success via x-api-key authentication matching gateway token
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	req.Header.Set("x-api-key", "gateway-secret-token")
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	if capturedApiKey != "sk-ant-real-upstream-key" {
		t.Errorf("capturedApiKey = %q, want upstream provider key", capturedApiKey)
	}
	if capturedAuth != "" {
		t.Errorf("capturedAuth = %q, want empty (Authorization must be cleared for Anthropic)", capturedAuth)
	}

	// 2. Reject when x-api-key is wrong and Authorization is absent
	reqWrong := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	reqWrong.Header.Set("x-api-key", "wrong-token")
	recWrong := httptest.NewRecorder()

	gateway.ServeAPIHTTP(recWrong, reqWrong)
	if recWrong.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", recWrong.Code)
	}
}

func TestGatewayAuthAnthropicTokenNotLeakedWhenNoAPIKey(t *testing.T) {
	var capturedApiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedApiKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		Auth: AuthConfig{
			Enable: true,
			Token:  "gateway-secret-token",
		},
		Providers: map[string]ProviderConfig{
			"claude-empty": {
				Name:         "claude-empty",
				BaseURL:      server.URL,
				APIKey:       "", // No key configured
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	req.Header.Set("x-api-key", "gateway-secret-token")
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	if capturedApiKey != "" {
		t.Errorf("capturedApiKey = %q, want empty (gateway token must not leak to upstream)", capturedApiKey)
	}
}

func TestTransparentAnthropicProxyWhenNoAPIKey(t *testing.T) {
	var capturedKey string
	var capturedVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKey = r.Header.Get("x-api-key")
		capturedVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"claude-byok": {
				Name:         "claude-byok",
				BaseURL:      server.URL,
				APIKey:       "",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	req.Header.Set("x-api-key", "sk-ant-client-custom-key")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request failed: %d", rec.Code)
	}
	if capturedKey != "sk-ant-client-custom-key" {
		t.Errorf("capturedKey = %q, want 'sk-ant-client-custom-key'", capturedKey)
	}
	if capturedVersion != "2023-06-01" {
		t.Errorf("capturedVersion = %q, want '2023-06-01'", capturedVersion)
	}
}

func TestStreamingProxyDoesNotCacheWhenExceedingMaxBodyBytes(t *testing.T) {
	for _, logResp := range []bool{false, true} {
		t.Run(fmt.Sprintf("LogResponseBody_%v", logResp), func(t *testing.T) {
			var upstreamHits int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&upstreamHits, 1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				chunks := []string{
					"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n",
					"data: {\"choices\":[{\"delta\":{\"content\":\" world!\"}}]}\n\n",
					"data: [DONE]\n\n",
				}
				for _, c := range chunks {
					_, _ = io.WriteString(w, c)
					if flusher != nil {
						flusher.Flush()
					}
				}
			}))
			defer server.Close()

			dbPath := filepath.Join(t.TempDir(), "stream_limit_test.db")
			storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
			if err != nil {
				t.Fatalf("OpenStorage failed: %v", err)
			}
			defer storage.Close()

			// MaxBodyBytes = 30 is smaller than the full stream (~100 bytes)
			cfg := Config{
				Logging: LoggingConfig{
					LogResponseBody: logResp,
				},
				Providers: map[string]ProviderConfig{
					"openai": {
						Name:         "openai",
						BaseURL:      server.URL,
						Weight:       1,
						Capabilities: []string{CapOpenAICompletions},
					},
				},
				Cache: CacheConfig{
					Enable:       true,
					MaxBodyBytes: 30,
				},
				Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
			}

			gateway, err := NewGateway(cfg, storage)
			if err != nil {
				t.Fatalf("NewGateway failed: %v", err)
			}

			body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`

			// 1. First streaming request
			req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
			rec1 := httptest.NewRecorder()
			gateway.ServeAPIHTTP(rec1, req1)

			if rec1.Code != http.StatusOK {
				t.Fatalf("req1 failed: %d", rec1.Code)
			}
			if !strings.Contains(rec1.Body.String(), "world!") {
				t.Errorf("client must receive full forwarded stream, got: %s", rec1.Body.String())
			}

			// Wait for worker goroutine to process exchange
			waitForRequestLogs(t, storage, 1)

			// 2. Second request should NOT hit cache because response exceeded MaxBodyBytes
			req2 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
			rec2 := httptest.NewRecorder()
			gateway.ServeAPIHTTP(rec2, req2)

			if rec2.Code != http.StatusOK {
				t.Fatalf("req2 failed: %d", rec2.Code)
			}
			if rec2.Header().Get("X-Cache") == "HIT" {
				t.Error("expected cache miss when stream response exceeds MaxBodyBytes, but got X-Cache: HIT")
			}
			if atomic.LoadInt32(&upstreamHits) != 2 {
				t.Errorf("upstreamHits = %d, want 2 (stream must not be cached)", upstreamHits)
			}
		})
	}
}

func TestIncompleteStreamNotCachedWithDefaultLogging(t *testing.T) {
	var upstreamHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// Upstream stream ends cleanly with EOF but WITHOUT the terminal [DONE] marker!
		chunks := []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello truncated\"}}]}\n\n",
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "incomplete_stream_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer storage.Close()

	// Default logging has LogResponseBody = true
	cfg := Config{
		Logging: LoggingConfig{
			LogResponseBody: true,
		},
		Providers: map[string]ProviderConfig{
			"openai": {
				Name:         "openai",
				BaseURL:      server.URL,
				Weight:       1,
				Capabilities: []string{CapOpenAICompletions},
			},
		},
		Cache: CacheConfig{
			Enable: true,
		},
		Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// 1. First streaming request
	req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 failed: %d", rec1.Code)
	}

	// Wait for worker goroutine to process exchange
	waitForRequestLogs(t, storage, 1)

	// 2. Second request: even with LogResponseBody=true, incomplete stream must NOT hit cache!
	req2 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 failed: %d", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") == "HIT" {
		t.Error("expected cache miss for incomplete stream even when LogResponseBody=true, but got X-Cache: HIT")
	}
	if atomic.LoadInt32(&upstreamHits) != 2 {
		t.Errorf("upstreamHits = %d, want 2 (incomplete stream must not be cached)", upstreamHits)
	}
}

func waitForRequestLogs(t testing.TB, storage *Storage, minCount int) []LogEntry {
	t.Helper()
	if minCount <= 0 {
		minCount = 1
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := storage.RequestLogs(1, 100)
		if err == nil && len(logs) >= minCount {
			return logs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for at least %d request log(s)", minCount)
	return nil
}
