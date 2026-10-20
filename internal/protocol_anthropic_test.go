package menshen

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnthropicProtocolPrepareBodyAndHeaders(t *testing.T) {
	proto := &AnthropicProtocol{}

	// 1. Headers: Authorization removed, x-api-key and anthropic-version injected
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer client-token")
	headers.Set("Other-Header", "val")

	provider := ProviderConfig{
		Name:    "claude",
		APIKey:  "sk-ant-test-key",
		BaseURL: "https://api.anthropic.com/v1",
	}

	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hello"}]}`)
	proto.ApplyHeaders(headers, provider)
	meta, err := proto.PrepareBody("/messages", body, provider, true)
	if err != nil {
		t.Fatalf("PrepareBody failed: %v", err)
	}

	if headers.Get("Authorization") != "" {
		t.Errorf("Authorization header should be deleted, got %q", headers.Get("Authorization"))
	}
	if headers.Get("x-api-key") != "sk-ant-test-key" {
		t.Errorf("x-api-key want %q, got %q", "sk-ant-test-key", headers.Get("x-api-key"))
	}
	if headers.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("anthropic-version want %q, got %q", "2023-06-01", headers.Get("anthropic-version"))
	}
	if meta.CacheKey == "" {
		t.Error("expected non-empty CacheKey")
	}

	// 2. Custom anthropic-version preserved
	headers2 := make(http.Header)
	headers2.Set("anthropic-version", "2024-01-01")
	proto.ApplyHeaders(headers2, provider)
	if headers2.Get("anthropic-version") != "2024-01-01" {
		t.Errorf("custom anthropic-version want %q, got %q", "2024-01-01", headers2.Get("anthropic-version"))
	}

	// 3. Model override & stream does NOT inject stream_options
	providerWithModel := ProviderConfig{
		Name:   "claude",
		APIKey: "sk-ant-test-key",
		Model:  "claude-3-opus-20240229",
	}
	streamBody := []byte(`{"model":"claude-3-5-sonnet-20241022","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	metaStream, err := proto.PrepareBody("/messages", streamBody, providerWithModel, true)
	if err != nil {
		t.Fatalf("PrepareRequest stream failed: %v", err)
	}
	if metaStream.EffectiveModel != "claude-3-opus-20240229" {
		t.Errorf("expected EffectiveModel overridden, got %q", metaStream.EffectiveModel)
	}
	if !metaStream.Stream {
		t.Error("expected Stream=true")
	}
	effectiveStr := string(metaStream.EffectiveBody)
	if strings.Contains(effectiveStr, "stream_options") {
		t.Errorf("Anthropic request must NOT contain stream_options, got: %s", effectiveStr)
	}
}

func TestAnthropicExtractUsage(t *testing.T) {
	proto := &AnthropicProtocol{}

	respBody := []byte(`{
		"id": "msg_01X9",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": [{"type": "text", "text": "Hello!"}],
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 40,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 15
		}
	}`)

	usage, err := proto.ExtractUsage("req-123", respBody)
	if err != nil {
		t.Fatalf("ExtractUsage error: %v", err)
	}
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if *usage.PromptTokens != 115 {
		t.Errorf("PromptTokens want 115, got %d", *usage.PromptTokens)
	}
	if *usage.CompletionTokens != 40 {
		t.Errorf("CompletionTokens want 40, got %d", *usage.CompletionTokens)
	}
	if *usage.TotalTokens != 155 {
		t.Errorf("TotalTokens want 155, got %d", *usage.TotalTokens)
	}
	if usage.CachedTokens == nil || *usage.CachedTokens != 15 {
		t.Errorf("CachedTokens want 15, got %v", usage.CachedTokens)
	}

	// Test with cache_creation_input_tokens > 0
	respBodyWithCreation := []byte(`{
		"id": "msg_02Y0",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": [{"type": "text", "text": "Hello!"}],
		"usage": {
			"input_tokens": 100,
			"output_tokens": 40,
			"cache_creation_input_tokens": 25,
			"cache_read_input_tokens": 15
		}
	}`)

	usage2, err := proto.ExtractUsage("req-456", respBodyWithCreation)
	if err != nil {
		t.Fatalf("ExtractUsage error: %v", err)
	}
	if usage2 == nil {
		t.Fatal("expected non-nil usage")
	}
	if *usage2.PromptTokens != 140 {
		t.Errorf("PromptTokens want 140, got %d", *usage2.PromptTokens)
	}
	if *usage2.CompletionTokens != 40 {
		t.Errorf("CompletionTokens want 40, got %d", *usage2.CompletionTokens)
	}
	if *usage2.TotalTokens != 180 {
		t.Errorf("TotalTokens want 180, got %d", *usage2.TotalTokens)
	}
	if usage2.CachedTokens == nil || *usage2.CachedTokens != 40 {
		t.Errorf("CachedTokens want 40, got %v", usage2.CachedTokens)
	}
}

func TestAnthropicStreamAuditor(t *testing.T) {
	proto := &AnthropicProtocol{}
	auditor := proto.NewStreamAuditor("req-stream-1")

	chunks := []string{
		"event: message_start\n",
		`data: {"type": "message_start", "message": {"id": "msg_1", "usage": {"input_tokens": 50, "cache_read_input_tokens": 10}}}` + "\n\n",
		"event: content_block_delta\n",
		`data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Hi"}}` + "\n\n",
		"event: message_delta\n",
		`data: {"type": "message_delta", "usage": {"output_tokens": 25}}` + "\n\n",
		"event: message_stop\n",
		`data: {"type": "message_stop"}` + "\n\n",
	}

	for _, chunk := range chunks {
		if err := auditor.Write([]byte(chunk)); err != nil {
			t.Fatalf("auditor.Write failed: %v", err)
		}
	}

	if !auditor.IsComplete() {
		t.Error("expected IsComplete() = true after message_stop")
	}

	usage := auditor.Usage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if *usage.PromptTokens != 60 {
		t.Errorf("PromptTokens want 60, got %d", *usage.PromptTokens)
	}
	if *usage.CompletionTokens != 25 {
		t.Errorf("CompletionTokens want 25, got %d", *usage.CompletionTokens)
	}
	if *usage.TotalTokens != 85 {
		t.Errorf("TotalTokens want 85, got %d", *usage.TotalTokens)
	}
	if usage.CachedTokens == nil || *usage.CachedTokens != 10 {
		t.Errorf("CachedTokens want 10, got %v", usage.CachedTokens)
	}
}

func TestAnthropicStreamAuditorTextContainingKeywords(t *testing.T) {
	proto := &AnthropicProtocol{}
	auditor := proto.NewStreamAuditor("req-stream-keywords")

	// Generated model content happens to contain keywords "message_start", "message_delta", and "message_stop"
	chunks := []string{
		"event: message_start\n",
		`data: {"type": "message_start", "message": {"id": "msg_1", "usage": {"input_tokens": 100}}}` + "\n\n",
		"event: content_block_delta\n",
		`data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Here is message_start and message_delta and message_stop"}}` + "\n\n",
		"event: message_delta\n",
		`data: {"type": "message_delta", "usage": {"output_tokens": 50}}` + "\n\n",
		"event: message_stop\n",
		`data: {"type": "message_stop"}` + "\n\n",
	}

	for _, chunk := range chunks {
		if err := auditor.Write([]byte(chunk)); err != nil {
			t.Fatalf("auditor.Write failed: %v", err)
		}
	}

	if !auditor.IsComplete() {
		t.Error("expected IsComplete() = true")
	}

	usage := auditor.Usage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if *usage.PromptTokens != 100 {
		t.Errorf("PromptTokens want 100, got %d", *usage.PromptTokens)
	}
	if *usage.CompletionTokens != 50 {
		t.Errorf("CompletionTokens want 50, got %d", *usage.CompletionTokens)
	}
	if *usage.TotalTokens != 150 {
		t.Errorf("TotalTokens want 150, got %d", *usage.TotalTokens)
	}
}

func TestAnthropicStreamAuditorIncomplete(t *testing.T) {
	proto := &AnthropicProtocol{}
	auditor := proto.NewStreamAuditor("req-stream-2")

	chunks := []string{
		"event: message_start\n",
		`data: {"type": "message_start", "message": {"id": "msg_1", "usage": {"input_tokens": 50}}}` + "\n\n",
		"event: content_block_delta\n",
		`data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Hi"}}` + "\n\n",
	}

	for _, chunk := range chunks {
		_ = auditor.Write([]byte(chunk))
	}

	if auditor.IsComplete() {
		t.Error("expected IsComplete() = false for truncated stream")
	}
}

func TestAnthropicProxyEndToEnd(t *testing.T) {
	var receivedKey, receivedVer, receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedKey = r.Header.Get("x-api-key")
		receivedVer = r.Header.Get("anthropic-version")
		receivedPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg_test",
			"type": "message",
			"model": "claude-3-5-sonnet-20241022",
			"content": [{"type": "text", "text": "Anthropic response"}],
			"usage": {"input_tokens": 12, "output_tokens": 8}
		}`))
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "anthropic_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"claude": {
				Name:         "claude",
				BaseURL:      server.URL,
				APIKey:       "sk-ant-secret",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
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

	// 1. First request -> hits upstream
	body := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if receivedKey != "sk-ant-secret" {
		t.Errorf("x-api-key want sk-ant-secret, got %q", receivedKey)
	}
	if receivedVer != "2023-06-01" {
		t.Errorf("anthropic-version want 2023-06-01, got %q", receivedVer)
	}
	if receivedPath != "/messages" {
		t.Errorf("path want /messages, got %q", receivedPath)
	}
	if rec.Header().Get("X-Cache") == "HIT" {
		t.Error("expected first request to be a cache miss")
	}

	// 2. Second request -> hits cache!
	req2 := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on cached request, got %d", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("expected second request to be X-Cache: HIT")
	}

	// 3. Check usage logged in storage
	time.Sleep(50 * time.Millisecond)
	logs, err := storage.RequestLogs(1, 10)
	if err != nil {
		t.Fatalf("RequestLogs failed: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected request logs in storage")
	}
	if logs[len(logs)-1].TotalTokens != 20 {
		t.Errorf("TotalTokens in storage want 20, got %d", logs[len(logs)-1].TotalTokens)
	}
}

func TestCapabilityRoutingAndFailover(t *testing.T) {
	serverOpenAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"openai"}}]}`))
	}))
	defer serverOpenAI.Close()

	serverClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"claude"}]}`))
	}))
	defer serverClaude.Close()

	dbPath := filepath.Join(t.TempDir(), "cap_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
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
		t.Fatalf("NewGateway failed: %v", err)
	}

	// 1. Explicitly routing an Anthropic request to OpenAI provider -> 400 Bad Request
	body := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/provider/openai/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for mismatched capability, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not support capability") {
		t.Errorf("error message missing capability details: %s", rec.Body.String())
	}

	// 2. Unpinned /messages automatically routes to claude (the only provider with anthropic-messages)
	req2 := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for auto-routed /messages, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "claude") {
		t.Errorf("expected response from claude provider, got: %s", rec2.Body.String())
	}

	// 3. Unpinned /chat/completions automatically routes to openai (the only provider with openai-completions)
	req3 := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	rec3 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for auto-routed /chat/completions, got %d: %s", rec3.Code, rec3.Body.String())
	}
	if !strings.Contains(rec3.Body.String(), "openai") {
		t.Errorf("expected response from openai provider, got: %s", rec3.Body.String())
	}
}

func TestAnthropicStreamingProxyEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			"event: message_start\n" + `data: {"type": "message_start", "message": {"id": "msg_stream", "usage": {"input_tokens": 15}}}` + "\n\n",
			"event: content_block_delta\n" + `data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Hello world"}}` + "\n\n",
			"event: message_delta\n" + `data: {"type": "message_delta", "usage": {"output_tokens": 5}}` + "\n\n",
			"event: message_stop\n" + `data: {"type": "message_stop"}` + "\n\n",
		}

		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "anthropic_stream_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"claude": {
				Name:         "claude",
				BaseURL:      server.URL,
				APIKey:       "sk-ant-secret",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
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

	body := `{"model":"claude-3-5-sonnet-20241022","stream":true,"messages":[{"role":"user","content":"stream test"}]}`

	// 1. First streaming request
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Cache") == "HIT" {
		t.Error("expected first streaming request to be cache miss")
	}
	if !strings.Contains(rec.Body.String(), "Hello world") {
		t.Errorf("stream response missing body, got: %s", rec.Body.String())
	}

	// Wait for worker goroutine to commit exchange transaction into SQLite
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := storage.RequestLogs(1, 10)
		if len(logs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 2. Second request should hit stream cache!
	req2 := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on cached stream, got %d", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("expected second streaming request to be X-Cache: HIT")
	}
	if !strings.Contains(rec2.Body.String(), "Hello world") {
		t.Errorf("cached stream response missing content, got: %s", rec2.Body.String())
	}
}

func TestExplicitProviderCacheHitWithoutCapability(t *testing.T) {
	var claudeRequests int32
	serverClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&claudeRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg_cached",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-5-sonnet-20241022",
			"content": [{"type": "text", "text": "cached response"}],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer serverClaude.Close()

	var openaiRequests int32
	serverOpenAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&openaiRequests, 1)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	defer serverOpenAI.Close()

	dbPath := filepath.Join(t.TempDir(), "cache_invariance_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"claude": {
				Name:         "claude",
				BaseURL:      serverClaude.URL,
				APIKey:       "sk-ant-test",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
			"openai": {
				Name:         "openai",
				BaseURL:      serverOpenAI.URL,
				APIKey:       "sk-oai-test",
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

	body := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"identical-prompt"}]}`

	// 1. Initial request to /messages routes to claude and populates cache
	req1 := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 failed: %d: %s", rec1.Code, rec1.Body.String())
	}
	if rec1.Header().Get("X-Cache") == "HIT" {
		t.Error("req1 should be a cache miss")
	}
	if atomic.LoadInt32(&claudeRequests) != 1 {
		t.Errorf("claudeRequests = %d, want 1", claudeRequests)
	}

	// Wait for worker goroutine to commit exchange transaction into SQLite
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := storage.RequestLogs(1, 10)
		if len(logs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 2. Request explicitly targeting 'openai' (/provider/openai/messages) with identical body.
	// Even though 'openai' does NOT support anthropic-messages, provider-agnostic cache MUST
	// return 200 OK with X-Cache: HIT and consume 0 upstream tokens.
	req2 := httptest.NewRequest(http.MethodPost, "/provider/openai/messages", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 should hit cache with 200 OK, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("req2 X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if !strings.Contains(rec2.Body.String(), "cached response") {
		t.Errorf("req2 unexpected body: %s", rec2.Body.String())
	}
	if atomic.LoadInt32(&openaiRequests) != 0 {
		t.Errorf("openaiRequests = %d, want 0 (cache hit must not reach upstream)", openaiRequests)
	}

	// 3. Cache miss on mismatched provider capability MUST return 400 Bad Request
	missBody := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"different-prompt"}]}`
	req3 := httptest.NewRequest(http.MethodPost, "/provider/openai/messages", strings.NewReader(missBody))
	rec3 := httptest.NewRecorder()
	gateway.ServeAPIHTTP(rec3, req3)

	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("req3 want 400 Bad Request on capability mismatch on cache miss, got %d", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), "does not support capability") {
		t.Errorf("req3 error message missing capability mismatch info: %s", rec3.Body.String())
	}
}

func TestAnthropicPassthroughHeaderInjection(t *testing.T) {
	var capturedAuth, capturedKey, capturedVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedKey = r.Header.Get("x-api-key")
		capturedVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer server.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"claude": {
				Name:         "claude",
				BaseURL:      server.URL,
				APIKey:       "sk-ant-test-key",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	// 1. Passthrough with custom anthropic-version
	req1 := httptest.NewRequest(http.MethodGet, "/provider/claude/models", nil)
	req1.Header.Set("Authorization", "Bearer client-secret")
	req1.Header.Set("anthropic-version", "2024-05-01")
	rec1 := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 failed: %d", rec1.Code)
	}
	if capturedAuth != "" {
		t.Errorf("capturedAuth = %q, want empty (client auth stripped)", capturedAuth)
	}
	if capturedKey != "sk-ant-test-key" {
		t.Errorf("capturedKey = %q, want %q", capturedKey, "sk-ant-test-key")
	}
	if capturedVersion != "2024-05-01" {
		t.Errorf("capturedVersion = %q, want %q", capturedVersion, "2024-05-01")
	}

	// 2. Passthrough without anthropic-version -> defaults to 2023-06-01
	req2 := httptest.NewRequest(http.MethodGet, "/provider/claude/models", nil)
	req2.Header.Set("Authorization", "Bearer client-secret")
	rec2 := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 failed: %d", rec2.Code)
	}
	if capturedAuth != "" {
		t.Errorf("capturedAuth = %q, want empty (client auth stripped)", capturedAuth)
	}
	if capturedKey != "sk-ant-test-key" {
		t.Errorf("capturedKey = %q, want %q", capturedKey, "sk-ant-test-key")
	}
	if capturedVersion != "2023-06-01" {
		t.Errorf("capturedVersion = %q, want default %q", capturedVersion, "2023-06-01")
	}
}

func TestMultiProviderModelOverrideSelection(t *testing.T) {
	var capturedModel string
	serverClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		capturedModel = req.Model

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg_override",
			"type": "message",
			"model": "claude-overridden",
			"content": [{"type": "text", "text": "ok"}],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer serverClaude.Close()

	serverOpenAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "openai should not be called", http.StatusInternalServerError)
	}))
	defer serverOpenAI.Close()

	dbPath := filepath.Join(t.TempDir(), "model_override_test.db")
	storage, err := OpenStorage(StorageConfig{SQLite: SQLiteConfig{Path: dbPath}})
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer storage.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"heavy-openai": {
				Name:         "heavy-openai",
				BaseURL:      serverOpenAI.URL,
				Model:        "gpt-4o-overridden",
				Weight:       1000, // Very heavy weight, would always be picked by global resolveProvider
				Capabilities: []string{CapOpenAICompletions},
			},
			"claude": {
				Name:         "claude",
				BaseURL:      serverClaude.URL,
				Model:        "claude-3-5-sonnet-overridden",
				Weight:       1,
				Capabilities: []string{CapAnthropicMessages},
			},
		},
		Storage: StorageConfig{SQLite: SQLiteConfig{Path: dbPath}},
	}

	gateway, err := NewGateway(cfg, storage)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	// Unpinned request to /messages -> capability routing must pick 'claude' BEFORE PrepareBody
	body := `{"model":"original-client-model","messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("request failed with status %d: %s", rec.Code, rec.Body.String())
	}
	if capturedModel != "claude-3-5-sonnet-overridden" {
		t.Errorf("upstream received model = %q, want 'claude-3-5-sonnet-overridden'", capturedModel)
	}

	// Verify logged model in storage corresponds to the selected provider's model
	deadline := time.Now().Add(2 * time.Second)
	var loggedModel string
	for time.Now().Before(deadline) {
		logs, err := storage.RequestLogs(1, 10)
		if err == nil && len(logs) > 0 {
			loggedModel = logs[0].Model
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if loggedModel != "claude-3-5-sonnet-overridden" {
		t.Errorf("logged model = %q, want 'claude-3-5-sonnet-overridden'", loggedModel)
	}
}

func TestDualCapabilityPassthroughAuth(t *testing.T) {
	var capturedAuth, capturedKey, capturedVersion, capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedKey = r.Header.Get("x-api-key")
		capturedVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer server.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"openrouter": {
				Name:         "openrouter",
				BaseURL:      server.URL,
				APIKey:       "sk-or-secret",
				Weight:       1,
				Capabilities: []string{CapOpenAICompletions, CapAnthropicMessages},
			},
		},
	}

	gateway, err := NewGateway(cfg, nil)
	if err != nil {
		t.Fatalf("NewGateway failed: %v", err)
	}

	// 1. Anthropic passthrough path -> must strip Authorization and set x-api-key + anthropic-version
	req1 := httptest.NewRequest(http.MethodPost, "/provider/openrouter/v1/messages/count_tokens", strings.NewReader(`{}`))
	req1.Header.Set("Authorization", "Bearer client-secret")
	rec1 := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 failed: %d", rec1.Code)
	}
	if capturedPath != "/v1/messages/count_tokens" {
		t.Errorf("capturedPath = %q, want /v1/messages/count_tokens", capturedPath)
	}
	if capturedAuth != "" {
		t.Errorf("capturedAuth = %q, want empty (Anthropic requires no Authorization header)", capturedAuth)
	}
	if capturedKey != "sk-or-secret" {
		t.Errorf("capturedKey = %q, want sk-or-secret", capturedKey)
	}
	if capturedVersion != "2023-06-01" {
		t.Errorf("capturedVersion = %q, want 2023-06-01", capturedVersion)
	}

	// 2. Non-Anthropic general passthrough path -> injects Bearer auth and x-api-key
	req2 := httptest.NewRequest(http.MethodGet, "/provider/openrouter/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer client-secret")
	rec2 := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 failed: %d", rec2.Code)
	}
	if capturedAuth != "Bearer sk-or-secret" {
		t.Errorf("capturedAuth = %q, want 'Bearer sk-or-secret'", capturedAuth)
	}
	if capturedKey != "sk-or-secret" {
		t.Errorf("capturedKey = %q, want sk-or-secret", capturedKey)
	}
}

func TestTransparentPassthroughWhenNoAPIKey(t *testing.T) {
	var capturedAuth, capturedKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer server.Close()

	// Provider has no APIKey configured
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"direct": {
				Name:         "direct",
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

	req := httptest.NewRequest(http.MethodGet, "/provider/direct/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-supplied-token")
	req.Header.Set("x-api-key", "client-supplied-ant-key")
	rec := httptest.NewRecorder()

	gateway.ServeAPIHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request failed: %d", rec.Code)
	}
	if capturedAuth != "Bearer client-supplied-token" {
		t.Errorf("capturedAuth = %q, want client token preserved", capturedAuth)
	}
	if capturedKey != "client-supplied-ant-key" {
		t.Errorf("capturedKey = %q, want client x-api-key preserved", capturedKey)
	}
}
