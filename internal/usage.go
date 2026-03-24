package aimenshen

import (
	"bytes"
	"encoding/json"
)

func ExtractUsage(requestID string, body []byte) (*UsageLog, error) {
	return extractUsageFromJSON(requestID, body)
}

type SSEUsageExtractor struct {
	requestID string
	pending   []byte
	usage     *UsageLog
}

func NewSSEUsageExtractor(requestID string) *SSEUsageExtractor {
	return &SSEUsageExtractor{requestID: requestID}
}

func (e *SSEUsageExtractor) Write(chunk []byte) error {
	e.pending = append(e.pending, chunk...)

	for {
		index := bytes.IndexByte(e.pending, '\n')
		if index < 0 {
			return nil
		}

		line := bytes.TrimSpace(e.pending[:index])
		e.pending = e.pending[index+1:]

		if usage, err := extractUsageFromSSELine(e.requestID, line); err != nil {
			return err
		} else if usage != nil {
			e.usage = usage
		}
	}
}

func (e *SSEUsageExtractor) Finalize() error {
	if len(bytes.TrimSpace(e.pending)) == 0 {
		return nil
	}

	usage, err := extractUsageFromSSELine(e.requestID, bytes.TrimSpace(e.pending))
	if err != nil {
		return err
	}
	if usage != nil {
		e.usage = usage
	}

	e.pending = nil
	return nil
}

func (e *SSEUsageExtractor) Usage() *UsageLog {
	return e.usage
}

func extractUsageFromSSELine(requestID string, line []byte) (*UsageLog, error) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil
	}

	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, nil
	}

	return extractUsageFromJSON(requestID, payload)
}

func extractUsageFromJSON(requestID string, body []byte) (*UsageLog, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil
	}

	return usageFromPayload(requestID, payload), nil
}

func usageFromPayload(requestID string, payload map[string]any) *UsageLog {
	usageObject, ok := payload["usage"].(map[string]any)
	if !ok {
		return nil
	}

	usage := &UsageLog{RequestID: requestID}
	var hasValue bool

	if value, ok := int64Value(usageObject["prompt_tokens"]); ok {
		usage.PromptTokens = &value
		hasValue = true
	}
	if value, ok := int64Value(usageObject["completion_tokens"]); ok {
		usage.CompletionTokens = &value
		hasValue = true
	}
	if value, ok := int64Value(usageObject["total_tokens"]); ok {
		usage.TotalTokens = &value
		hasValue = true
	}

	if details, ok := usageObject["prompt_tokens_details"].(map[string]any); ok {
		if value, ok := int64Value(details["cached_tokens"]); ok {
			usage.CachedTokens = &value
			hasValue = true
		}
	}

	if !hasValue {
		return nil
	}

	return usage
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return number, true
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}
