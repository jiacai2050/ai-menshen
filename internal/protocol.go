package menshen

import (
	"net/http"
)

const (
	CapOpenAICompletions = "openai-completions"
	CapAnthropicMessages = "anthropic-messages"
)

// Protocol encapsulates the protocol-specific rules for an LLM API format.
type Protocol interface {
	// Name returns the protocol capability identifier ("openai-completions", "anthropic-messages").
	Name() string

	// ApplyHeaders sets upstream authentication headers and protocol version headers.
	ApplyHeaders(headers http.Header, p ProviderConfig)

	// PrepareBody inspects the request body, rewrites model/options if needed,
	// and computes the canonical CacheKey.
	PrepareBody(path string, body []byte, p ProviderConfig, cacheEnabled bool) (RequestMeta, error)

	// ExtractUsage extracts token metrics from a non-streaming JSON response.
	ExtractUsage(reqID string, body []byte) (*UsageLog, error)

	// NewStreamAuditor creates a stateful SSE stream listener for this protocol.
	NewStreamAuditor(reqID string) StreamAuditor
}

// StreamAuditor observes SSE chunks during streaming proxying.
type StreamAuditor interface {
	// Write processes an incoming SSE chunk as it is forwarded to the client.
	Write(chunk []byte) error

	// IsComplete reports whether the stream concluded cleanly with its official
	// end-of-stream signal (e.g. data: [DONE] or event: message_stop).
	IsComplete() bool

	// Usage returns the aggregated token metrics extracted across the stream.
	Usage() *UsageLog
}

var defaultProtocolRoutes = map[string]Protocol{
	"/chat/completions":    &OpenAIProtocol{},
	"/responses":           &OpenAIProtocol{},
	"/v1/chat/completions": &OpenAIProtocol{},
	"/v1/responses":        &OpenAIProtocol{},
	"/v1/messages":         &AnthropicProtocol{},
	"/messages":            &AnthropicProtocol{},
}

func findProtocol(path string) Protocol {
	return defaultProtocolRoutes[path]
}
