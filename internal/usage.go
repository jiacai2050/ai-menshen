package aimenshen

import (
	"bytes"
	"encoding/json"
)

var (
	sseNewline     = []byte("\n")
	sseDataPrefix  = []byte("data:")
	sseDonePayload = []byte("[DONE]")
	usageFieldKey  = []byte(`"usage"`)
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

// Write buffers chunks just long enough to split complete lines. The leftover
// is compacted to the front of the buffer so the backing array is reused
// instead of reallocating on every chunk.
func (e *SSEUsageExtractor) Write(chunk []byte) error {
	e.pending = append(e.pending, chunk...)

	rest := e.pending
	for {
		line, remainder, ok := bytes.Cut(rest, sseNewline)
		if !ok {
			break
		}
		rest = remainder

		if usage, err := extractUsageFromSSELine(e.requestID, bytes.TrimSpace(line)); err != nil {
			return err
		} else if usage != nil {
			e.usage = usage
		}
	}
	e.pending = e.pending[:copy(e.pending, rest)]
	return nil
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

	e.pending = e.pending[:0]
	return nil
}

func (e *SSEUsageExtractor) Usage() *UsageLog {
	return e.usage
}

func extractUsageFromSSELine(requestID string, line []byte) (*UsageLog, error) {
	if !bytes.HasPrefix(line, sseDataPrefix) {
		return nil, nil
	}

	payload := bytes.TrimSpace(line[len(sseDataPrefix):])
	if len(payload) == 0 || bytes.Equal(payload, sseDonePayload) {
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
	// Fast path: without a "usage" key there is nothing to extract, so skip
	// the full JSON decode. The check can only produce false positives (e.g.
	// the key appearing inside generated text), which fall through to the
	// regular decode below.
	if !bytes.Contains(body, usageFieldKey) {
		return nil, nil
	}

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
