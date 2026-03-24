package gateway

import "time"

type RequestLog struct {
	ID          string
	CreatedAt   time.Time
	Path        string
	Model       string
	CacheKey    string
	RequestBody string
}

type ResponseLog struct {
	RequestID         string
	StatusCode        int
	ResponseBody      string
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
	ResponseBody string
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

type RequestMeta struct {
	OriginalBody   []byte
	EffectiveBody  []byte
	EffectiveModel string
	CacheKey       string
	Stream         bool
	JSON           bool
}
