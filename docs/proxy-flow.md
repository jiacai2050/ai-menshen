# Proxy Request Flow

## Routing

```mermaid
flowchart TD
    REQ[Request] --> NOISE{Browser noise?}
    NOISE -->|Yes| N404[404]
    NOISE -->|No| AUTH{Auth enabled?}
    AUTH -->|Fail| N401[401]
    AUTH -->|Pass| ROUTE{Route}
    ROUTE -->|"/ or /assets or /__report"| STATIC[Dashboard / Assets / Reports]
    ROUTE -->|Non-auditable| PASS[proxyPassthrough]
    ROUTE -->|"/chat/completions or /responses"| AUDIT[Auditable Flow]
```

## Auditable Request Flow

```mermaid
flowchart TD
    AUDIT[ReadAll body] --> PARSE["ParseRequest(body)<br/>JSON decode once"]
    PARSE --> FWD["forwardWithFailover(r, meta, &requestLog)"]

    FWD --> PREP["PrepareForProvider(provider)<br/>model override + marshal + cache key"]
    PREP --> CACHE{Cache hit?<br/>primary only}
    CACHE -->|Yes| CACHED[Return Cached]
    CACHE -->|No| UPSTREAM["forwardUpstream(provider)"]
    UPSTREAM --> OK{Status < 500?}
    OK -->|Yes| RESP[Return Resp]
    OK -->|No| NEXT{More providers?}
    NEXT -->|Yes| PREP
    NEXT -->|No| ERR[Return Err]

    CACHED --> R1[serveCachedResponse]
    ERR --> R2[502 + save log]
    RESP --> STREAM{Stream?}
    STREAM -->|Yes| R3[proxyStream: chunk + flush]
    STREAM -->|No| R4[ReadAll + write response]
    R3 --> SAVE[Extract usage + save exchange]
    R4 --> SAVE
```

### ParseRequest (once, provider-independent)

Single JSON decode. Extracts `stream` and `model`, injects `stream_options` for streaming. Retains the parsed payload map for reuse.

### forwardWithFailover

Iterates providers (single if failover disabled). For each provider:

1. `PrepareForProvider` — applies model override (clones map if needed), builds cache key, marshals body. Fills `requestLog` fields.
2. Cache lookup (primary provider only) — hit returns immediately.
3. `forwardUpstream` — builds URL, injects provider auth/headers, sends request.
4. Non-5xx → return success. 5xx/error → try next provider.

### Response handling (caller)

`ServeHTTP` dispatches on the `forwardResult`:
- `Cached` → serve from cache
- `Err` → 502
- `Resp` + stream → `proxyStream` (chunked streaming with SSE usage extraction)
- `Resp` + non-stream → read full body, extract usage, write response

## Design Principles

- **Single JSON decode** — `ParseRequest` parses once, payload map reused across providers
- **One marshal per provider attempt** — only in `PrepareForProvider`
- **No shared mutation** — model override clones the payload map
- **`forwardWithFailover` never touches `ResponseWriter`** — returns data, caller writes
