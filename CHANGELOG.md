# Changelog

## v1.2.0 (2026-03-29)

### Added
- **Passthrough Mode**: Silent proxying for non-auditable routes (e.g., non-completion APIs) to reduce storage noise and overhead
- **Stream Caching**: Full support for caching and replaying SSE (Server-Sent Events) responses for `/chat/completions` and `/completions`
- **Installation Proxy**: Added `--china` flag to `install.sh` to use a proxy for downloads in China
- Automatic injection of `stream_options = { "include_usage": true }` for stream requests when the client does not provide it, ensuring usage data is captured

### Changed
- **Breaking**: Config section `[http_client]` renamed to `[upstream]` to better reflect its role in proxying
- **Storage**: Response bodies are now stored as `BLOB` in SQLite for better performance and non-UTF-8 compatibility

### Fixed
- Stream response logging: Ensure partial/failed streams are marked as `502` or `206` to prevent incorrect cache hits

## v1.1.0 (2026-03-27)

### Changed
- **Breaking**: `[storage]` config restructured — `sqlite_path` moved to `[storage.sqlite] path`
- `[cache]` `max_body_bytes` default changed from 1 MiB to 5 MiB
- `[cache]` `enable` default is now `true` (matching README)
- `main.go` refactored: extracted `run()` function so `defer storage.Close()` always executes
- Simplified graceful shutdown logic in `main.go`
- `Storage.Close()` guarded with `atomic.Bool` to prevent panic on concurrent sends
- `docs/plan.md` renamed to `docs/roadmap.md`, removed completed items

### Added
- `[http_client]` config section with `timeout` for upstream request timeout (default 300s, 0 = no limit)
- `[cache]` `max_age` option for cache TTL (seconds, 0 = never expire)
- `[logging]` section: `log_request_body` and `log_response_body` to control body persistence
- `logInfo` / `logError` helpers with `[INFO]` / `[ERROR]` prefixes
- Request ID (`[req-xxx]`) in all proxy log messages for traceability
- Upstream error logs now include elapsed time

### Fixed
- `storage.Close()` could panic if `SaveExchange` raced with channel close

## v1.0.1 (2026-03-26)

### Added
- Cache TTL support (`max_age` in `[cache]`)
- Restructured config: `[storage.sqlite]` nested config with env var support for `path`
- Automatic log retention via background worker (`retention_days`)
- Configurable authentication for proxy (Bearer) and dashboard (Basic Auth)
- Updated `README.md` and `configs/example.toml` with all new options
