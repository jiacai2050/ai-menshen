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

type usageResponse struct {
	Usage *usageData `json:"usage"`
}

type usageData struct {
	PromptTokens     int64               `json:"prompt_tokens"`
	CompletionTokens int64               `json:"completion_tokens"`
	TotalTokens      int64               `json:"total_tokens"`
	PromptDetails    *promptTokenDetails `json:"prompt_tokens_details"`
}

type promptTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

func extractUsageFromJSON(requestID string, body []byte) (*UsageLog, error) {
	var resp usageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil
	}

	if resp.Usage == nil {
		return nil, nil
	}

	usage := &UsageLog{
		RequestID:        requestID,
		PromptTokens:     &resp.Usage.PromptTokens,
		CompletionTokens: &resp.Usage.CompletionTokens,
		TotalTokens:      &resp.Usage.TotalTokens,
	}

	if resp.Usage.PromptDetails != nil {
		usage.CachedTokens = &resp.Usage.PromptDetails.CachedTokens
	}

	return usage, nil
}
