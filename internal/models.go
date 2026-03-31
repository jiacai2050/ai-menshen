package aimenshen

import "time"

type RequestLog struct {
	ID          string
	CreatedAt   time.Time
	Path        string
	Model       string
	CacheKey    string
	RequestBody []byte
}

type ResponseLog struct {
	RequestID         string
	StatusCode        int
	ResponseBody      []byte
	DurationMS        int64
	FromCache         bool
	CacheHitRequestID string
}

type UsageLog struct {
	RequestID        string
	PromptTokens     *int64
	CompletionTokens *int64
	TotalTokens      *int64
	CachedTokens     *int64
}

type CachedResponse struct {
	RequestID    string
	StatusCode   int
	ResponseBody []byte
}

type DailyUsage struct {
	Date             string `json:"date"`
	TotalTokens      int64  `json:"total_tokens"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	RequestCount     int64  `json:"request_count"`
}

type UsageSummary struct {
	TotalRequests    int64 `json:"total_requests"`
	CacheHits        int64 `json:"cache_hits"`
	TotalTokens      int64 `json:"total_tokens"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
}

type ModelUsageReport struct {
	Model            string `json:"model"`
	RequestCount     int64  `json:"request_count"`
	CacheHits        int64  `json:"cache_hits"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
}

type LogEntry struct {
	ID                  string `json:"id"`
	CreatedAt           int64  `json:"created_at"`
	Model               string `json:"model"`
	Path                string `json:"path"`
	StatusCode          int    `json:"status_code"`
	DurationMS          int64  `json:"duration_ms"`
	TotalTokens         int64  `json:"total_tokens"`
	RequestBodyPreview  string `json:"request_preview"`
	ResponseBodyPreview string `json:"response_preview"`
}

type LogDetail struct {
	LogEntry
	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
}

type RequestMeta struct {
	OriginalBody   []byte
	EffectiveModel string
	Stream         bool
	Payload        map[string]any // parsed JSON payload, nil if body is not JSON
}
