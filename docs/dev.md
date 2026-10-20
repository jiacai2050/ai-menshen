# Development & Performance Benchmarking

This guide describes how to run and analyze performance benchmarks for **menshen**.

## Benchmark Overview

The repository includes micro-benchmarks under [`internal/perf_bench_test.go`](../internal/perf_bench_test.go) focusing on the critical proxy hot paths:

| Benchmark | Description | Key Focus Area |
| :--- | :--- | :--- |
| `BenchmarkAnalyzeRequestNoOverride` | Analyzes request payload without model override | Baseline JSON decoding and cache key generation |
| `BenchmarkAnalyzeRequestModelOverride` | Analyzes request payload with provider model override | Rewriting JSON body by splicing raw fields |
| `BenchmarkExtractUsageNoUsage` | Evaluates SSE chunk payload with no `usage` field | Fast path short-circuiting (`bytes.Contains`) |
| `BenchmarkSSEExtractorStream` | Simulates a 100-chunk SSE stream passing through the extractor | Line splitting and line buffer reuse (`copy`) |
| `BenchmarkAnalyzeBigNoOverride` | Analyzes large payload (200 messages) without model override | Scaling with large request sizes |
| `BenchmarkAnalyzeBigModelOverride` | Analyzes large payload (200 messages) with model override | Splicing large payload without full re-encoding |

---

## Running Benchmarks

### 1. Basic Benchmark Run

To run all benchmarks in the `internal` package:

```bash
go test -bench=. ./internal
```

> **Note**: Go runs matching unit tests before benchmarks by default. To skip unit tests and run only benchmarks, add `-run=^$`:
> ```bash
> go test -run=^$ -bench=. ./internal
> ```

### 2. Include Memory Allocations (`-benchmem`)

To display heap allocations per operation (`B/op` and `allocs/op`), include the `-benchmem` flag:

```bash
go test -run=^$ -bench=. -benchmem ./internal
```

**Example Output:**
```
BenchmarkAnalyzeRequestNoOverride-12       41938      28567 ns/op     23058 B/op      441 allocs/op
BenchmarkAnalyzeRequestModelOverride-12    40275      30192 ns/op     25059 B/op      454 allocs/op
BenchmarkExtractUsageNoUsage-12          8753868        133.1 ns/op        0 B/op        0 allocs/op
BenchmarkSSEExtractorStream-12             75802      16217 ns/op       224 B/op        2 allocs/op
BenchmarkAnalyzeBigNoOverride-12            4363     266688 ns/op    220092 B/op     3509 allocs/op
BenchmarkAnalyzeBigModelOverride-12         4110     274256 ns/op    240592 B/op     3521 allocs/op
```

### 3. Run Specific Benchmarks

Filter by benchmark name with a regex pattern:

```bash
# Run only request analysis benchmarks
go test -run=^$ -bench=BenchmarkAnalyze ./internal -benchmem

# Run only SSE usage extraction benchmarks
go test -run=^$ -bench=BenchmarkExtract|BenchmarkSSE ./internal -benchmem
```

### 4. Improve Measurement Stability

For more reliable numbers (reducing CPU throttling or background noise):

- **Increase benchmark duration**: `-benchtime=3s`
- **Run multiple iterations**: `-count=5`

```bash
go test -run=^$ -bench=. -benchmem -benchtime=3s -count=5 ./internal
```

---

## Performance Comparison with `benchstat`

When optimizing or refactoring, compare the baseline and the modified code using `benchstat`:

1. Install `benchstat`:
   ```bash
   go install golang.org/x/perf/cmd/benchstat@latest
   ```

2. Record baseline results (on main branch):
   ```bash
   go test -run=^$ -bench=. -benchmem -count=5 ./internal > old.txt
   ```

3. Record results after your changes (on feature branch):
   ```bash
   go test -run=^$ -bench=. -benchmem -count=5 ./internal > new.txt
   ```

4. Compare:
   ```bash
   benchstat old.txt new.txt
   ```

---

## Profiling CPU and Memory

To identify bottlenecks or unexpected allocations, capture pprof profiles:

```bash
# Generate CPU and Memory profile files
go test -run=^$ -bench=BenchmarkAnalyzeBig -benchmem \
  -cpuprofile=cpu.pprof -memprofile=mem.pprof ./internal
```

### Inspecting Profiles

- **Web UI** (requires Graphviz):
  ```bash
  go tool pprof -http=:8080 cpu.pprof
  ```

- **Interactive CLI**:
  ```bash
  go tool pprof cpu.pprof
  (pprof) top20
  (pprof) list AnalyzeRequest
  ```
