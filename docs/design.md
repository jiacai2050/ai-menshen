# Request Audit, Cache Reuse, and Token Reporting Design

## Problem

The service originally only provided transparent proxying and basic logging. It could not persist request/response data or report token usage by model. The goal is to add SQLite-backed request auditing, reusable response caching, and token reporting while preserving transparent proxy behavior as much as possible.

## Approach Overview

Introduce a local SQLite storage layer that records normalized request content, upstream responses, token usage, latency, and cache hit information. When the same request appears again, the service can reuse a previously stored response instead of calling the upstream provider again, reducing token consumption. The stored data also enables model-level token usage reports.

## Design Principles

- Preserve transparent proxy behavior by default.
- Prioritize auditability and reporting before expanding cache reuse.
- Strictly separate safely reusable requests from requests that should never be cached.
- Support correct stream handling, but keep stream cache replay out of scope for now.
- Prefer the Go standard library where practical; use a focused SQLite driver only where needed.

## Scope

This design includes:

- SQLite persistence for requests and responses
- token/usage extraction and storage
- normalized request fingerprinting for cache hits
- model-level reporting
- configuration, schema, and request flow

This design does not currently promise:

- distributed caching
- shared storage across instances
- precise billing calculation
- universal normalization across every OpenAI-compatible provider

## Core Design

### 1. Request Auditing

Each request creates one `request_logs` record with only the essential fields:

- `id`
- `created_at`
- `path`
- `model`
- `cache_key`
- `request_body`

Notes:

- `request_body` supports both auditing and possible replay/debugging.
- `cache_key` is the core deduplication and cache-hit key.
- `path` and `model` are enough for most operational reporting.

### 2. Response Auditing

Each request has one `response_logs` record with:

- `request_id`
- `status_code`
- `response_body`
- `duration_ms`
- `from_cache`
- `cache_hit_request_id`

Notes:

- `response_body` supports auditing and response replay.
- `from_cache` and `cache_hit_request_id` preserve cache lineage.
- Response headers are intentionally excluded from the first schema version.

### 3. Token Usage Extraction

The `usage_logs` table stores:

- `request_id`
- `prompt_tokens`
- `completion_tokens`
- `total_tokens`
- `cached_tokens`

Extraction strategy:

- Non-stream: extract `usage` from the final JSON response body.
- Stream: extract `usage` from SSE `data:` events when the stream includes a usage payload.
- If the upstream response has no usage data, the usage row may be absent while the request/response rows are still stored.

### 4. Request Normalization and Cache Keys

Cache reuse is based on semantic equality, not raw JSON string equality.

Suggested cache key inputs:

- request path such as `/chat/completions`
- `model`
- output-affecting fields such as `messages`, `input`, `temperature`, `top_p`, `max_tokens`, `tools`, and `response_format`
- `stream` (cache replay currently only allows `false`)

Suggested exclusions:

- `user`
- client-defined tracing fields
- irrelevant headers that do not affect model output

Normalization strategy:

- parse the JSON body into a generic structure
- sort map keys deterministically
- remove fields that should not affect cache identity
- serialize again and hash with `sha256`

Notes:

- Ordered arrays such as `messages`, `tools`, and `response_format` must keep their original order.
- Floating-point values should remain unchanged.

### 5. Reusable Cache Strategy

Cache replay is intentionally limited to safe non-stream requests.

Suggested conditions:

- `POST /chat/completions` or another explicitly supported endpoint
- `stream=false`
- previous response had `status_code=200`
- request body normalized successfully
- request shows no obvious real-time or non-deterministic behavior

Suggested default exclusions:

- `stream=true`
- upload endpoints
- large image/audio payloads in the first iteration
- tool-calling flows with possible external side effects

Cache replay behavior:

- return the historical `response_body`
- mark the new `response_logs` row as `from_cache=true`
- point `cache_hit_request_id` to the original request

### 6. Reporting

The first version exposes reporting through code and the HTTP endpoint `GET /__report/models`.

Suggested dimensions:

- by model: request count, cache hits, prompt tokens, completion tokens, total tokens, cached tokens
- by time window: today, last 7 days, last 30 days
- by path: `/chat/completions`, `/responses`, and other supported endpoints

Example SQL:

```sql
SELECT
  model,
  COUNT(*) AS request_count,
  SUM(CASE WHEN from_cache THEN 1 ELSE 0 END) AS cache_hits,
  SUM(COALESCE(prompt_tokens, 0)) AS prompt_tokens,
  SUM(COALESCE(completion_tokens, 0)) AS completion_tokens,
  SUM(COALESCE(total_tokens, 0)) AS total_tokens,
  SUM(COALESCE(cached_tokens, 0)) AS cached_tokens
FROM request_logs rl
LEFT JOIN response_logs rs ON rs.request_id = rl.id
LEFT JOIN usage_logs ul ON ul.request_id = rl.id
GROUP BY model
ORDER BY total_tokens DESC;
```

## Recommended Schema

This is intentionally a minimal schema focused on three jobs: record requests, replay safe responses, and report token usage.

```sql
CREATE TABLE request_logs (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  path TEXT NOT NULL,
  model TEXT,
  cache_key TEXT,
  request_body TEXT
);

CREATE TABLE response_logs (
  request_id TEXT PRIMARY KEY,
  status_code INTEGER,
  response_body TEXT,
  duration_ms INTEGER NOT NULL,
  from_cache INTEGER NOT NULL DEFAULT 0,
  cache_hit_request_id TEXT,
  FOREIGN KEY(request_id) REFERENCES request_logs(id)
);

CREATE TABLE usage_logs (
  request_id TEXT PRIMARY KEY,
  prompt_tokens INTEGER,
  completion_tokens INTEGER,
  total_tokens INTEGER,
  cached_tokens INTEGER,
  FOREIGN KEY(request_id) REFERENCES request_logs(id)
);

CREATE INDEX idx_request_logs_cache_key ON request_logs(cache_key);
CREATE INDEX idx_request_logs_created_at ON request_logs(created_at);
CREATE INDEX idx_request_logs_model ON request_logs(model);
```

## Project Structure

Follow a simple Go project layout:

- `cmd/ai-menshen`: thin binary entrypoint
- `internal`: private application code
- `configs`: example configuration files

`internal` should contain at least:

- `config.go`: CLI parsing and TOML config structures
- `proxy.go`: proxying, upstream calls, and report handlers
- `storage.go`: SQLite bootstrap and persistence
- `normalize.go`: request normalization and cache key generation
- `usage.go`: usage extraction
- `cache.go`: cache checks and replay rules
- `models.go`: shared structs

## Request Processing Flow

### Non-stream

1. Read the request body and restore it.
2. Parse the model, detect streaming mode, normalize the request, and generate `cache_key`.
3. Check whether the request can use cache replay.
4. If there is a hit, replay the stored response and record the hit.
5. Otherwise forward the request upstream.
6. Read the full upstream response.
7. Extract usage.
8. Persist request, response, and usage to SQLite.
9. Return the response to the client.

### Stream

1. Detect `stream=true`.
2. Create the request log record.
3. Forward the request upstream.
4. Stream SSE data to the client while copying the raw stream into memory.
5. After the stream ends, extract `usage` from the captured SSE events.
6. Persist request, response, and usage to SQLite.
7. Do not attempt stream cache replay yet.

## Technical Choices

- SQLite driver: prefer a focused pure-Go driver to avoid CGO.
- Concurrency: use WAL mode for better read/write behavior.
- Serialization: keep stored bodies as text for simple inspection and replay.
- IDs: generate lightweight unique request IDs in-process.

## Configuration

Use a TOML configuration file such as `config.toml`.

Keep a minimal CLI:

- `-config`: path to the TOML config file
- `-h` / `--help`: print usage information

Suggested config fields:

- `listen`: listen address such as `:8080`
- `providers`: upstream provider list; the current version uses only the first item
- `storage.sqlite_path`: SQLite file path
- `cache.enable`: enable cache replay
- `cache.max_body_bytes`: maximum cacheable response size
- `verbose`: enable verbose logs
- `logging.log_request_body`: store request bodies
- `logging.log_response_body`: store response bodies
- `storage.retention_days`: retention period

Recommended TOML example:

```toml
listen = ":8080"
verbose = false

[[providers]]
base_url = "https://api.openai.com/v1"
api_key = "sk-..."
model = "gpt-4.1"

[storage]
sqlite_path = "./data/ai-menshen.db"
retention_days = 30

[cache]
enable = false
max_body_bytes = 1048576

[logging]
log_request_body = true
log_response_body = true
```

Notes:

- `providers` is an array to keep room for future routing strategies.
- The current implementation only reads `providers[0]`.
- If a provider sets `model`, that value overrides the incoming client model.

## Risks and Considerations

### 1. Privacy and Security

- Requests and responses may contain sensitive user data.
- The SQLite database must be treated as an audit store.
- Real upstream API keys must never be persisted.

### 2. Cache Correctness

- Not every similar-looking request should be cached.
- A loose cache key can return incorrect results.
- Prefer conservative cache behavior over aggressive reuse.

### 3. Stream Complexity

- Streams must be flushed incrementally while still preserving a copy for auditing and usage extraction.
- Stream audit is supported, but stream cache replay remains intentionally out of scope for now.

### 4. Limited Schema

- The minimal schema is easier to operate, but it does trade away some debugging detail.
- If needed later, the schema can grow to include headers, hashes, or raw usage payloads.

## Notes

- Full request/response body storage is currently supported and useful for replay and debugging.
- Stream auditing is implemented; stream cache replay is still intentionally deferred.
