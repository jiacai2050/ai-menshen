package menshen

import (
	"testing"
)

func TestOpenAIResponsesStreamingUsage(t *testing.T) {
	proto := &OpenAIProtocol{}
	auditor := proto.NewStreamAuditor("req-resp-stream")

	chunks := [][]byte{
		[]byte("data: {\"type\": \"response.created\"}\n\n"),
		[]byte("data: {\"type\": \"response.output_item.added\"}\n\n"),
		[]byte("data: {\"type\": \"response.completed\", \"response\": {\"usage\": {\"prompt_tokens\": 18, \"completion_tokens\": 32, \"total_tokens\": 50, \"prompt_tokens_details\": {\"cached_tokens\": 5}}}}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}

	for _, chunk := range chunks {
		if err := auditor.Write(chunk); err != nil {
			t.Fatalf("auditor.Write failed: %v", err)
		}
	}

	if !auditor.IsComplete() {
		t.Error("expected auditor.IsComplete() = true")
	}

	usage := auditor.Usage()
	if usage == nil {
		t.Fatal("expected non-nil usage from response.completed")
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 18 {
		t.Errorf("prompt tokens = %v, want 18", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 32 {
		t.Errorf("completion tokens = %v, want 32", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 50 {
		t.Errorf("total tokens = %v, want 50", usage.TotalTokens)
	}
	if usage.CachedTokens == nil || *usage.CachedTokens != 5 {
		t.Errorf("cached tokens = %v, want 5", usage.CachedTokens)
	}
}
