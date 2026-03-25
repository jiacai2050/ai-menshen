# Cache Design in ai-menshen

This document describes the design and implementation of the request-response caching system in **ai-menshen**.

## 1. Overview
The goal of the caching system is to reduce latency and costs by instantly replaying responses for identical non-streaming requests. The system relies on a deterministic, collision-resistant **Cache Key** generated from the request's semantics.

## 2. Cache Key Construction
A `CacheKey` is a SHA-256 hash of a normalized JSON representation of the request. The input to the hash includes:

1.  **Request Path**: Ensures that different API endpoints (e.g., `/chat/completions` vs `/embeddings`) do not share the same cache, even if their payloads happen to be identical.
2.  **Request Payload**: The actual parameters sent by the client.

### Mathematical Definition
`CacheKey = SHA256( CanonicalJSON({ "path": path, "request": normalized_payload }) )`

## 3. Normalization Logic (Canonicalization)
To ensure that functionally identical requests produce the same key regardless of formatting, the system performs the following steps:

### A. Key Sorting (Determinism)
JSON objects are unordered by nature. `ai-menshen` recursively sorts all keys in alphabetical order during serialization.
- **Example**: `{"a":1, "b":2}` and `{"b":2, "a":1}` produce the same hash.

### B. Field Exclusion (Semantics)
Certain fields are part of the HTTP request but do not affect the model's output content. These are excluded to increase the cache hit rate:
- `user`: Identifiers for the end-user.
- `stream_options`: Options like `include_usage` which change the SSE framing but not the logical content.
- `stream`: Explicitly excluded if `false` (streaming requests are never cached in the current version).

### C. Floating Point Stability
To avoid discrepancies between different CPU architectures or Go versions, floating-point numbers (e.g., `temperature`, `top_p`) are formatted using a fixed deterministic decimal representation (`strconv.FormatFloat` with 'f' and precision -1).

## 4. Performance Optimizations
The cache key calculation is designed for high-throughput environments:

- **Zero Deep-Cloning**: The normalization happens "on-the-fly" during serialization. We do not create a copy of the request tree in memory.
- **Buffer Pooling**: Uses `sync.Pool` to reuse `bytes.Buffer` and `sha256.Hash` objects, significantly reducing GC pressure.
- **Streaming Hashing**: Data is written directly into the hashing engine via a pooled buffer, minimizing memory allocations.

## 5. Storage & Retrieval
- **Backend**: SQLite `response_logs` table.
- **Lookup**: An index on `request_logs.cache_key` ensures O(1) retrieval.
- **Rule**: Only `200 OK` non-stream responses are eligible for caching.
- **Size Limit**: Configurable `max_body_bytes` prevents extremely large responses from bloating the database or memory.

## 6. Limitations & Future Work
- **Stream Caching**: Currently not supported due to the complexity of replaying SSE chunks.
- **Header Sensitivity**: Currently, HTTP headers (like `OpenAI-Organization`) are ignored. If headers start affecting upstream behavior, they should be added to the normalization logic.
- **Multitenancy**: Cache is currently global. In a multi-user environment, a user identifier hash should be included in the `CacheKey`.
