package menshen

import (
	"bytes"
	"net/http"
)

type OpenAIProtocol struct{}

func (p *OpenAIProtocol) Name() string {
	return CapOpenAICompletions
}

func (p *OpenAIProtocol) ApplyHeaders(headers http.Header, provider ProviderConfig) {
	// Only override Authorization if the provider explicitly configures an API key.
	// When provider.APIKey is empty, preserve the client's incoming Authorization
	// header to support transparent proxying and BYOK (Bring Your Own Key).
	// If gateway auth is enabled (cfg.Auth.Enable = true), gateway token leakage
	// is already prevented in copyRequestHeaders via isProviderHeader.
	if provider.APIKey != "" {
		headers.Del(authHeaderName)
		headers.Set(authHeaderName, "Bearer "+provider.APIKey)
	}
}

func (p *OpenAIProtocol) PrepareBody(path string, body []byte, provider ProviderConfig, cacheEnabled bool) (RequestMeta, error) {
	return AnalyzeRequest(path, body, provider, cacheEnabled)
}

func (p *OpenAIProtocol) ExtractUsage(reqID string, body []byte) (*UsageLog, error) {
	return ExtractUsage(reqID, body)
}

func (p *OpenAIProtocol) NewStreamAuditor(reqID string) StreamAuditor {
	return &OpenAIStreamAuditor{
		requestID: reqID,
	}
}

type OpenAIStreamAuditor struct {
	requestID  string
	pending    []byte
	usage      *UsageLog
	isComplete bool
}

func (a *OpenAIStreamAuditor) Write(chunk []byte) error {
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

func (a *OpenAIStreamAuditor) processLine(line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, sseDataPrefix) {
		return nil
	}

	payload := bytes.TrimSpace(trimmed[len(sseDataPrefix):])
	if bytes.Equal(payload, sseDonePayload) {
		a.isComplete = true
		return nil
	}

	if usage, err := extractUsageFromJSON(a.requestID, payload); err != nil {
		return err
	} else if usage != nil {
		a.usage = usage
	}
	return nil
}

func (a *OpenAIStreamAuditor) IsComplete() bool {
	if !a.isComplete && len(bytes.TrimSpace(a.pending)) > 0 {
		_ = a.processLine(a.pending)
	}
	return a.isComplete
}

func (a *OpenAIStreamAuditor) Usage() *UsageLog {
	if len(bytes.TrimSpace(a.pending)) > 0 {
		_ = a.processLine(a.pending)
	}
	return a.usage
}
