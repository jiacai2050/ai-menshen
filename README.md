# ai-menshen

ai-menshen (门神) is a lightweight, local-first Go proxy for OpenAI-compatible APIs. It stands in front of upstream providers to keep auditing, caching, and API keys under your absolute control.

> Pairs great with [OpenClaw](https://openclaw.ai/) 🦞.

## Core Features

- **Auth Injection**: Keep real upstream API keys safe in your local config.
- **Model Override**: Force specific models (e.g., `gpt-4o`) regardless of client request.
- **Auditing**: Log every request, response, and token usage to a local **SQLite** database.
- **Smart Cache**: Instant replay for matching non-stream requests to save costs.
- **Stream Support**: Full SSE support with real-time token usage extraction.
- **Usage Reports**: Model-level token totals via `GET /__report/models`.

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

    style EXTERNAL fill:#f9f9f9,stroke:#ccc,stroke-dasharray: 5 5
    style LOCAL fill:#e6f3ff,stroke:#0066cc,stroke-width:2px
    style G fill:#fff,stroke:#0066cc,stroke-width:2px
```

## Quick Start

### 1. Configure & Run
```bash
cp configs/example.toml config.toml
# Edit config.toml with your api_key and base_url
go run ./cmd/ai-menshen
```

### 2. Connect Your Client
Point your OpenAI client to `http://localhost:8080`.

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080", api_key="local-placeholder")

response = client.chat.completions.create(
    model="any-model", # Will be overridden if a provider model is set in config.toml ([[providers]].model)
    messages=[{"role": "user", "content": "Hello!"}]
)
```

### 3. Check Reports
```bash
curl http://localhost:8080/__report/models
```

## Configuration Guide

Create a `config.toml` file to customize ai-menshen's behavior. You can find a complete template in [configs/example.toml](configs/example.toml).

### Global Settings
- `listen`: The address and port to listen on (default: `:8080`).
- `verbose`: Enable debug logging to stdout (default: `false`).

### `[[providers]]` (Array)
- `base_url`: Upstream OpenAI-compatible API endpoint (required).
- `api_key`: Your API key for the provider. Supports environment variables.
- `headers`: A map of custom HTTP headers to inject into upstream requests. Values support environment variables.
- `model`: Optional. If set, it overrides the `model` field in all incoming requests.

**Example: Cloudflare AI Gateway (BYOK)**
```toml
[[providers]]
base_url = "https://gateway.ai.cloudflare.com/v1/ACCOUNT_ID/GATEWAY_NAME/openai"
headers = { "cf-aig-authorization" = "Bearer ${DEEPSEEK_API_KEY}" }
```

### `[storage]`
- `sqlite_path`: Path to the SQLite database file (default: `./data/ai-menshen.db`).
- `retention_days`: Number of days to keep audit logs (default: `30`).

### `[cache]`
- `enable`: Enable response caching for non-stream requests (default: `true`).
- `max_body_bytes`: Maximum response size to cache in bytes (default: `1MB`).

### `[logging]`
- `log_request_body`: Store the full request body in SQLite (default: `true`).
- `log_response_body`: Store the full response body in SQLite (default: `true`).

## Environment Variables

ai-menshen supports environment variable expansion (e.g., `${API_KEY}` or `$API_KEY`) for sensitive fields in `config.toml`. This allows you to keep your secrets out of the configuration file.

Supported fields:
- `api_key`
- Values within the `headers` map

Example:
```toml
[[providers]]
api_key = "${OPENAI_API_KEY}"
headers = { "X-Custom-Auth" = "$CUSTOM_TOKEN" }
```
