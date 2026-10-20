package menshen

import (
	"bytes"
	"encoding/json"
	"net/http"
)

type AnthropicProtocol struct{}

func (p *AnthropicProtocol) Name() string {
	return CapAnthropicMessages
}

func (p *AnthropicProtocol) ApplyHeaders(headers http.Header, provider ProviderConfig) {
	// 1. Clear Authorization header to prevent upstream rejection
	headers.Del(authHeaderName)

	// 2. Set x-api-key if provider has API key
	if provider.APIKey != "" {
		headers.Set("x-api-key", provider.APIKey)
	}

	// 3. Set default anthropic-version if not provided
	if headers.Get("anthropic-version") == "" {
		headers.Set("anthropic-version", "2023-06-01")
	}
}

func (p *AnthropicProtocol) PrepareBody(path string, body []byte, provider ProviderConfig, cacheEnabled bool) (RequestMeta, error) {
	return AnalyzeAnthropicRequest(path, body, provider, cacheEnabled)
}

func (p *AnthropicProtocol) ExtractUsage(reqID string, body []byte) (*UsageLog, error) {
	if !bytes.Contains(body, usageFieldKey) {
		return nil, nil
	}

	var resp struct {
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil, nil
	}

	promptTokens := resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens
	outputTokens := resp.Usage.OutputTokens
	totalTokens := promptTokens + outputTokens
	cachedTokens := resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens

	usage := &UsageLog{
		RequestID:        reqID,
		PromptTokens:     &promptTokens,
		CompletionTokens: &outputTokens,
		TotalTokens:      &totalTokens,
	}

	if cachedTokens > 0 {
		usage.CachedTokens = &cachedTokens
	}

	return usage, nil
}

func (p *AnthropicProtocol) NewStreamAuditor(reqID string) StreamAuditor {
	return &AnthropicStreamAuditor{
		requestID: reqID,
	}
}

type AnthropicStreamAuditor struct {
	requestID       string
	pending         []byte
	inputTokens     int64
	outputTokens    int64
	cachedTokens    int64
	hasInputTokens  bool
	hasOutputTokens bool
	isComplete      bool
}

func (a *AnthropicStreamAuditor) Write(chunk []byte) error {
	a.pending = append(a.pending, chunk...)

	rest := a.pending
	for {
		line, remainder, ok := bytes.Cut(rest, sseNewline)
		if !ok {
			break
		}
		rest = remainder

		if err := a.processLine(line); err != nil {
			return err
		}
	}
	a.pending = a.pending[:copy(a.pending, rest)]
	return nil
}

func (a *AnthropicStreamAuditor) processLine(line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		event := bytes.TrimSpace(trimmed[len("event:"):])
		if bytes.Equal(event, []byte("message_stop")) {
			a.isComplete = true
		}
		return nil
	}

	if !bytes.HasPrefix(trimmed, sseDataPrefix) {
		return nil
	}

	payload := bytes.TrimSpace(trimmed[len(sseDataPrefix):])
	if len(payload) == 0 {
		return nil
	}

	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}

	switch ev.Type {
	case "message_stop":
		a.isComplete = true
	case "message_start":
		a.inputTokens = ev.Message.Usage.InputTokens + ev.Message.Usage.CacheCreationInputTokens + ev.Message.Usage.CacheReadInputTokens
		a.cachedTokens = ev.Message.Usage.CacheReadInputTokens + ev.Message.Usage.CacheCreationInputTokens
		a.hasInputTokens = true
	case "message_delta":
		a.outputTokens = ev.Usage.OutputTokens
		a.hasOutputTokens = true
	}

	return nil
}

func (a *AnthropicStreamAuditor) IsComplete() bool {
	if !a.isComplete && len(bytes.TrimSpace(a.pending)) > 0 {
		_ = a.processLine(a.pending)
	}
	return a.isComplete
}

func (a *AnthropicStreamAuditor) Usage() *UsageLog {
	if len(bytes.TrimSpace(a.pending)) > 0 {
		_ = a.processLine(a.pending)
	}

	if !a.hasInputTokens && !a.hasOutputTokens {
		return nil
	}

	input := a.inputTokens
	output := a.outputTokens
	total := input + output
	usage := &UsageLog{
		RequestID:        a.requestID,
		PromptTokens:     &input,
		CompletionTokens: &output,
		TotalTokens:      &total,
	}

	if a.cachedTokens > 0 {
		cached := a.cachedTokens
		usage.CachedTokens = &cached
	}

	return usage
}
