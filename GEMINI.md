# ai-menshen Context

This project is a lightweight OpenAI-compatible proxy service written in Go.

## Current Focus

ai-menshen now does more than basic auth injection:

- forwards OpenAI-compatible requests to one configured upstream provider chosen per request
- automatically fails over to another provider on network errors, HTTP 5xx, or 429
- keeps the real upstream API key in a local TOML config file
- optionally overrides the client `model` with the selected provider's `model`
- records both non-stream and stream requests and responses in SQLite
- extracts token usage from normal JSON responses and stream SSE events when `usage` is present
- optionally reuses matching cached non-stream responses
- exposes a model-level usage report at `GET /__report/models`

## Architecture Overview

The repository follows a simple Go layout:

- `main.go`: thin binary entrypoint at the root
- `internal/config.go`: TOML config loading and CLI parsing
- `internal/proxy.go`: request handling, upstream forwarding, cache replay, and reports
- `internal/storage.go`: SQLite setup and persistence
- `internal/normalize.go`: request normalization and cache key generation
- `internal/usage.go`: usage extraction from JSON responses
- `internal/cache.go`: cacheability rules
- `internal/models.go`: shared structs
- `configs/config.example.toml`: example runtime config

## Configuration

Runtime configuration comes from a TOML file such as `config.toml`. The repository keeps an example at `configs/config.example.toml`.

The binary entrypoint supports a minimal CLI:

- `-config`: path to the TOML config file
- `-h` / `--help`: print usage information

Important fields:

- `listen`: local listen address
- `providers`: provider array; one provider is selected per request using weight
  - `base_url`: upstream OpenAI-compatible base URL
  - `api_key`: upstream API key
  - `model`: optional model override for forwarded requests
  - `weight`: request-distribution weight; if omitted it is treated as `0`, and `0` disables the provider. At least one provider must have a weight greater than `0`.
- `storage.sqlite.path`: SQLite database path
- `cache.enable`: enables non-stream cache replay
- `cache.max_body_bytes`: maximum cached response body size
- `logging.log_request_body`: whether to store request bodies
- `logging.log_response_body`: whether to store response bodies
- `failover.enable`: enables automatic failover to other providers (default `true`)
- `verbose`: enables debug logging to stdout

## Runtime Behavior

- **Failover**: when a provider returns a network error, HTTP 5xx, or 429, the proxy automatically retries with the remaining active providers in config order. The first provider is chosen by weighted random; subsequent attempts are deterministic. For streaming requests, failover only happens at the connection stage — once SSE data has begun flowing to the client the stream is not retried. If all providers fail (or failover is disabled), the last upstream response is passed through as-is rather than replaced with a synthetic 502. Failover applies only to auditable paths (`/chat/completions`, `/responses`); passthrough paths use a single provider.
- Non-stream requests are candidates for auditing, usage extraction, and cache replay.
- Stream requests are also audited and can contribute usage stats, but cache replay remains non-stream only.
- Cloudflare AI Gateway still uses `cf-aig-authorization` instead of `Authorization`.
- Model-level aggregated usage is available at `GET /__report/models`.
- The SQLite schema is intentionally minimal:
  - `request_logs`
  - `response_logs`
  - `usage_logs`

## Development Conventions

- Keep the code small and practical.
- Prefer standard library components unless a focused dependency is justified.
- Preserve transparent proxy behavior for normal client usage.
- Keep the entrypoint thin under `cmd/` and place private implementation under `internal/`.
