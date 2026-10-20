package menshen

import (
	"strings"
	"testing"
)

var benchBody = []byte(`{"model":"gpt-4o","stream":false,"temperature":0.7,"messages":[` +
	strings.Repeat(`{"role":"user","content":"hello world, this is a reasonably long chat message."},`, 20) +
	`{"role":"user","content":"final"}]}`)

var benchSSELine = []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hello world"},"finish_reason":null}]}` + "\n")

func BenchmarkAnalyzeRequestNoOverride(b *testing.B) {
	provider := ProviderConfig{}
	for b.Loop() {
		if _, err := AnalyzeRequest("/chat/completions", benchBody, provider, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnalyzeRequestModelOverride(b *testing.B) {
	provider := ProviderConfig{Model: "gpt-4.1"}
	for b.Loop() {
		if _, err := AnalyzeRequest("/chat/completions", benchBody, provider, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtractUsageNoUsage(b *testing.B) {
	payload := benchSSELine[len("data: ") : len(benchSSELine)-1]
	for b.Loop() {
		if _, err := ExtractUsage("req-bench", payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSSEExtractorStream(b *testing.B) {
	for b.Loop() {
		e := (&OpenAIProtocol{}).NewStreamAuditor("req-bench")
		for i := 0; i < 100; i++ {
			if err := e.Write(benchSSELine); err != nil {
				b.Fatal(err)
			}
		}
	}
}

var benchBigBody = []byte(`{"model":"gpt-4o","stream":false,"temperature":0.7,"messages":[` +
	strings.Repeat(`{"role":"user","content":"hello world, this is a long chat message with some payload to make it realistic."},`, 200) +
	`{"role":"user","content":"final"}]}`)

func BenchmarkAnalyzeBigNoOverride(b *testing.B) {
	provider := ProviderConfig{}
	for b.Loop() {
		if _, err := AnalyzeRequest("/chat/completions", benchBigBody, provider, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnalyzeBigModelOverride(b *testing.B) {
	provider := ProviderConfig{Model: "gpt-4.1"}
	for b.Loop() {
		if _, err := AnalyzeRequest("/chat/completions", benchBigBody, provider, true); err != nil {
			b.Fatal(err)
		}
	}
}
