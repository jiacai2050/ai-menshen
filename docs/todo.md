# TODO

This file tracks follow-up work that is intentionally kept out of the current PR so the weighted-provider change stays focused.

## Proxy hot-path allocation follow-ups

1. Precompute the expected API bearer token at startup instead of rebuilding `"Bearer " + token` on each authenticated request.
2. Parse provider `base_url` values once during startup and reuse the parsed form during upstream forwarding.
3. Remove `strings.ToLower(...)` from hop-by-hop header checks in the proxy path.
4. Avoid per-request `[]any` allocation in cache lookup argument building.

## Lower-priority ideas

1. Profile streaming buffer reuse impact further before adding more pooling.
2. Revisit request normalization and canonical JSON generation only if profiling shows it is still a major hotspot.

## Cache correctness with weighted providers

1. Revisit cache key scoping now that provider selection happens per request.
2. Cached responses can currently be replayed across different providers because the cache key is derived from request path + payload, not provider identity.
3. Consider including a stable provider identifier in the cache key, or disabling cache when multiple active providers are configured with different upstreams.
