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
    AUDIT[ReadAll body] --> PICK["pickProviders()<br/>weighted-random primary + failover order"]
    PICK --> LOOP["For each provider"]

    LOOP --> ANALYZE["AnalyzeRequest(body, provider)<br/>model override + stream_options + cache key"]
    ANALYZE --> CACHE{Cache hit?<br/>primary only}
    CACHE -->|Yes| R1[serveCachedResponse]
    CACHE -->|No| UPSTREAM["forwardUpstream(provider)"]
    UPSTREAM --> OK{Status < 500<br/>and not 429?}
    OK -->|Yes| RESP[Got response]
    OK -->|No| NEXT{More providers?}
    NEXT -->|Yes| LOOP
    NEXT -->|No| R2[502 + save log]

    RESP --> STREAM{Stream?}
    STREAM -->|Yes| R3[proxyStream: chunk + flush]
    STREAM -->|No| R4[ReadAll + write response]
    R3 --> SAVE[Extract usage + save exchange]
    R4 --> SAVE
```

### Provider selection

`pickProviders()` builds the provider list for each request:

1. Filter out providers with `weight = 0` (disabled).
2. `weightedPick()` — select a primary provider randomly, proportional to weights.
3. Return primary first, remaining active providers follow as failover candidates.
4. Single active provider → no allocation, returned directly.

### Failover loop

For each provider in the list:

1. `AnalyzeRequest` — applies model override, injects `stream_options`, builds cache key, marshals body.
2. Cache lookup (primary provider only, i == 0) — hit returns immediately.
3. `forwardUpstream` — builds URL, injects provider auth/headers, sends request.
4. Status < 500 and not 429 → break with success. Otherwise → close body, try next provider.

### Response handling

After the loop, `ServeHTTP` checks the result:
- `resp == nil` → all providers failed, 502
- Stream → `proxyStream(resp)` — chunked streaming with SSE usage extraction
- Non-stream → read full body, extract usage, write response

## Design Principles

- **`AnalyzeRequest` is idempotent** — called per provider attempt, no shared state mutation
- **Weighted load balancing + automatic failover** — single mechanism, no separate config toggle
- **`proxyStream` receives an already-established `resp`** — failover happens before streaming begins
- **Duration uses `time.Since(startedAt)`** — reflects true user-perceived latency across all attempts
