# ai-menshen Context

This project is a lightweight OpenAI-compatible proxy service written in Go.

## Current Focus

ai-menshen now does more than basic auth injection:

- forwards OpenAI-compatible requests to a configured upstream provider
- keeps the real upstream API key in a local TOML config file
- optionally overrides the client `model` with `providers[0].model`
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
- `providers`: provider array; the current implementation only uses the first item
  - `base_url`: upstream OpenAI-compatible base URL
  - `api_key`: upstream API key
  - `model`: optional model override for forwarded requests
- `storage.sqlite_path`: SQLite database path
- `cache.enable`: enables non-stream cache replay
- `cache.max_body_bytes`: maximum cached response body size
- `logging.log_request_body`: whether to store request bodies
- `logging.log_response_body`: whether to store response bodies
- `verbose`: enables debug logging to stdout

## Runtime Behavior

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
