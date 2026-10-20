package menshen

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

type usageResponse struct {
	Usage    *usageData `json:"usage"`
	Response *struct {
		Usage *usageData `json:"usage"`
	} `json:"response"`
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

	u := resp.Usage
	if u == nil && resp.Response != nil {
		u = resp.Response.Usage
	}
	if u == nil {
		return nil, nil
	}

	usage := &UsageLog{
		RequestID:        requestID,
		PromptTokens:     &u.PromptTokens,
		CompletionTokens: &u.CompletionTokens,
		TotalTokens:      &u.TotalTokens,
	}

	if u.PromptDetails != nil {
		usage.CachedTokens = &u.PromptDetails.CachedTokens
	}

	return usage, nil
}
