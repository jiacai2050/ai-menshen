# menshen Context

This project is a lightweight AI gateway service written in Go.

## Design Invariants

- **The response cache is provider-agnostic on purpose.** Cache keys are built from the request path (after stripping any `/provider/<name>` prefix) plus the request body, never from the provider name. A response cached from one provider may therefore be replayed for a request that is routed to another provider, including one pinned explicitly via `/provider/<name>`: an identical request should cost zero upstream tokens no matter which provider produced the cached answer. Do not add the provider name to `buildCacheKey`, and do not skip cache replay when a provider is selected explicitly.

## Development Conventions

- Keep the code small and practical.
- Prefer standard library components unless a focused dependency is justified.
- Preserve transparent proxy behavior for normal client usage.
- Keep the entrypoint thin under `cmd/` and place private implementation under `internal/`.
- Run `make fmt` after modifying code to ensure consistent formatting.
