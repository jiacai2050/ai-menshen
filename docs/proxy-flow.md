# Proxy Request Flow

## Routing

```
Request → ServeHTTP
  ├─ Browser noise (favicon, robots.txt) → 404
  ├─ Auth check → Basic Auth for UI, Bearer Token for API
  ├─ Dashboard / Assets / Reports → static files or DB queries
  ├─ Non-auditable path → proxyPassthrough (forward as-is via primary provider)
  └─ Auditable path (/chat/completions, /responses) → see below
```

## Auditable Request Flow

```
ReadAll(body) → ParseRequest → PrepareForProvider(primary) → Cache lookup
  ├─ Cache hit → serveCachedResponse → done
  └─ Cache miss
       ├─ stream  → proxyStream  → forwardWithFailover → stream response
       └─ regular → ServeHTTP    → forwardWithFailover → buffered response
```

### Step 1: Parse (once)

`ParseRequest(body)` — single JSON decode, extracts `stream` and `model`, injects `stream_options` for streaming. Retains the parsed payload map for reuse.

### Step 2: Prepare + Cache (primary provider only)

`PrepareForProvider(path, meta, provider)` — applies model override (clones map if needed), builds cache key, marshals body. Cache is checked only for the primary provider.

### Step 3: Forward

`forwardWithFailover(r, meta)` — pure forwarding logic:
- Iterates providers (just primary if failover disabled)
- Calls `PrepareForProvider` per provider, then `forwardUpstream`
- Returns on first non-5xx response, or error if all fail
- No cache logic, no response writing

### Step 4: Handle Response (caller)

The caller (`ServeHTTP` or `proxyStream`) owns all response writing and log storage:
- Updates `requestLog` with the final provider's model/cacheKey/body
- Reads response, extracts usage, saves exchange, writes to client

## Design Principles

- **Single JSON decode** — `ParseRequest` parses once, payload map reused across providers
- **One marshal per provider attempt** — only in `PrepareForProvider`
- **Cache at the outer layer** — checked once for primary provider before entering failover
- **No shared mutation** — model override clones the map
- **Clean separation** — `forwardWithFailover` never touches `ResponseWriter`
