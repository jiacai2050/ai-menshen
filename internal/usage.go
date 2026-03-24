package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

func ExtractUsage(requestID string, body []byte) (*UsageLog, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil
	}

	return usageFromPayload(requestID, payload), nil
}

func ExtractUsageFromSSE(requestID string, body []byte) (*UsageLog, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	maxTokenSize := 1024 * 1024
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payloadText := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payloadText == "" || payloadText == "[DONE]" {
			continue
		}

		decoder := json.NewDecoder(strings.NewReader(payloadText))
		decoder.UseNumber()

		var payload map[string]any
		if err := decoder.Decode(&payload); err != nil {
			continue
		}

		if usage := usageFromPayload(requestID, payload); usage != nil {
			return usage, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return nil, nil
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
