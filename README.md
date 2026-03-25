# ai-menshen

ai-menshen (门神) is a local-first proxy for OpenAI-compatible APIs. It sits in front of your upstream providers to handle **Auth Injection (BYOK)**, **Model Overriding**, **Usage Auditing**, and **Caching**—keeping your API keys and logs strictly under your control.

## Showcase

| Overview & Trends | Audit Logs |
| :---: | :---: |
| ![Overview](docs/screenshot-overview.webp) | ![Logs](docs/screenshot-logs.webp) |

> *Standalone Go binary. No external dependencies except SQLite.*

```mermaid
graph TD
    subgraph EXTERNAL [External World]
        direction LR
        Client([Clients / OpenClaw 🦞])
        Upstream(["Upstream API<br>(OpenAI/DeepSeek...)"])
    end

    subgraph LOCAL [Local Environment]
        direction TB
        G["ai-menshen<br>(Standalone Go binary)"]

        DB[(SQLite)]
        CFG[config.toml]

        G --- DB
        G --- CFG
    end

    Client ==>|OpenAI API| G
    G ==>|Auth Injection| Upstream

    style EXTERNAL fill:none,stroke:#ccc,stroke-dasharray: 5 5
    style LOCAL fill:#f0f7ff,stroke:#0066cc,stroke-width:2px
    style G fill:#0066cc,color:#fff
    style DB fill:#fff,stroke:#ddd
    style CFG fill:#fff,stroke:#ddd
```

## Quick Start

1. **Run**:
   ```bash
   cp configs/example.toml config.toml
   # Add your upstream API key to config.toml
   go run ./cmd/ai-menshen -config config.toml
   ```

2. **Connect**:
   Point your client to `http://localhost:8080`. For streaming usage auditing, ensure `stream_options={"include_usage": True}` is set.

3. **Report**:
   ```bash
   curl http://localhost:8080/__report/models
   ```

## Configuration Guide

Customize `config.toml` (template: [configs/example.toml](configs/example.toml)). `api_key` and `headers` values support **Environment Variables** (e.g., `${KEY}`).

| Section | Field | Description | Default |
| :--- | :--- | :--- | :--- |
| **Global** | `listen` | Local bind address | `:8080` |
| **Providers** | `base_url` | Upstream endpoint (Required) | - |
| | `api_key` | Upstream key (Supports env) | - |
| | `headers` | Custom headers (e.g., `{ "cf-aig-authorization" = "Bearer..." }`) | `{}` |
| | `model` | Force override request model | - |
| **Storage** | `sqlite_path` | SQLite database location | `./data/ai-menshen.db` |
| **Cache** | `enable` | Cache non-stream 200 responses | `true` |
| **Logging** | `log_request_body` | Persist full requests in DB | `true` |
