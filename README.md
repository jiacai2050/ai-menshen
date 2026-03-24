# ai-menshen

ai-menshen is a lightweight, local-first Go proxy for OpenAI-compatible APIs, designed to keep auditing, caching, and usage reporting under your control.

The name **menshen** (门神) comes from Chinese culture. A *menshen* is a "door guardian" or "gate guardian" figure placed at an entrance to protect what is behind it. That fits this project well: ai-menshen stands in front of an upstream AI provider, guards the real API key, and decides how requests should pass through.

> Pairs great with [OpenClaw](https://openclaw.ai/) 🦞.

## Features

- **Provider config via TOML**: load upstream settings from `config.toml`.
- **Auth injection**: keep real upstream API keys on the server.
- **Provider model override**: optionally replace the client-supplied `model` with `providers[0].model`.
- **Auditing**: persist request, response, latency, and token usage to SQLite.
- **Optional cache replay**: reuse previous non-stream responses for matching requests.
- **Usage reporting**: query model-level token totals at `GET /__report/models`.
- **Stream support**: `stream=true` is forwarded incrementally, recorded in SQLite, and can contribute usage stats when the stream includes usage data.

## How It Works

```mermaid
%%{init: {'theme': 'base', 'themeVariables': { 'primaryColor': '#e6f3ff'}}}%%
flowchart TD
    A["1. Client Request In"] --> B["2. Transform (Auth & Model Inject)"]
    B --> C{"3. Decision: Cache Hit?"}
    
    C -- "Yes (Fast)" --> D["4. Replay from SQLite"]
    C -- "No (Slow)" --> E["4. Forward to Upstream"]
    
    D & E --> F["5. Audit (Usage & Latency)"]
    F --> G["6. Serve Response Out"]

    %% Styling
    style B fill:#cce5ff,stroke:#0066cc,stroke-width:2px
    style C fill:#fff3cd,stroke:#ffc107,stroke-width:2px
    style D fill:#e6ffe6,stroke:#009900,stroke-width:2px
    style F fill:#d1e7dd,stroke:#198754,stroke-width:2px
```

In short: ai-menshen acts as a smart gateway that injects credentials, optionally overrides models, reuses cached responses from SQLite, and logs every exchange for usage reporting.

## Architecture

```mermaid
%%{init: {'theme': 'base', 'themeVariables': { 'primaryColor': '#e6f3ff'}}}%%
flowchart LR
    subgraph EXTERNAL [External World]
        C["Clients (SDK / curl)"]
        U["Upstream (OpenAI / DeepSeek)"]
    end

    subgraph LOCAL [Your Local Environment]
        direction TB
        G["ai-menshen (Go Binary)"]
        CFG["config.toml"]
        DB[("SQLite (Logs & Cache)")]
        
        G -.-> CFG
        G -.-> DB
    end

    C <== "OpenAI API" ==> G
    G <== "Auth Injection" ==> U

    %% Styling
    style EXTERNAL fill:#f9f9f9,stroke:#ccc,stroke-dasharray: 5 5
    style LOCAL fill:#e6f3ff,stroke:#0066cc,stroke-width:2px
    style G fill:#fff,stroke:#0066cc,stroke-width:2px
```

This structural view highlights `ai-menshen` as the secure local bridge, keeping all sensitive data (API keys, audit logs, and cache) strictly within **Your Local Environment**.

## Configuration

Create a `config.toml` file, or start from `configs/config.example.toml`:

```toml
listen = ":8080"
verbose = false

[[providers]]
base_url = "https://api.openai.com/v1"
api_key = "sk-..."
# Optional: override the client's model before forwarding upstream.
# model = "gpt-4.1"

[storage]
sqlite_path = "./data/ai-menshen.db"
retention_days = 30

[cache]
enable = true
max_body_bytes = 1048576

[logging]
log_request_body = true
log_response_body = true
```

Notes:

- `providers` is an array for future expansion, but the current implementation only uses `providers[0]`.
- If `providers[0].model` is set, that value replaces the incoming request's `model`.
- SQLite data is stored at `storage.sqlite_path`.
- The repository keeps the example config in `configs/config.example.toml`.

## Quick Start

### 1. Run

```bash
cp configs/config.example.toml ./config.toml

go run ./cmd/ai-menshen

# show CLI help
go run ./cmd/ai-menshen -h

# pass a custom config path
go run ./cmd/ai-menshen -config ./config.toml
```

### 2. Build

```bash
make build
```

### 3. Connect a Client

#### OpenAI Python SDK

ai-menshen still speaks the normal OpenAI-compatible HTTP API. Clients only need to point `base_url` at the local service; the upstream key stays in `config.toml`.

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080",
    api_key="local-placeholder"
)

response = client.chat.completions.create(
    model="gpt-4.1-mini",
    messages=[{"role": "user", "content": "Hello via ai-menshen!"}]
)
print(response.choices[0].message.content)
```

#### curl

```bash
curl http://localhost:8080/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"Hi!"}]}'
```

## Reports

Fetch aggregated usage by model:

```bash
curl http://localhost:8080/__report/models
```

Example response:

```json
[
  {
    "model": "gpt-4.1",
    "request_count": 12,
    "cache_hits": 3,
    "prompt_tokens": 240,
    "completion_tokens": 1900,
    "total_tokens": 2140,
    "cached_tokens": 80
  }
]
```

## Current Scope

- Auditing applies to both non-stream and stream requests.
- Stream requests are forwarded incrementally and can record usage from SSE `data:` events that include a `usage` object.
- Cache replay is still limited to supported **non-stream** JSON endpoints such as `/chat/completions` and `/responses`.
