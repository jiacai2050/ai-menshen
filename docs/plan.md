# Future Plans & Ideas

This document outlines potential features and improvements for **ai-menshen**.

## 1. Reliability: Multi-Provider Failover
- **Current State**: Only `providers[0]` is utilized.
- **Goal**: Implement automatic failover if the primary provider returns 5xx errors or timeouts.
- **Benefit**: High availability for users relying on multiple API upstream channels.

## 2. Cost Management: Token-to-Price Mapping
- **Goal**: Add optional pricing configuration (e.g., price per 1k tokens) for specific models.
- **Benefit**: Include `estimated_cost` in the usage reports (`GET /__report/models`) for easier budgeting.

## 3. Visualization: Built-in Dashboard
- **Goal**: Use Go's `embed` to serve a lightweight, single-page HTML dashboard.
- **Benefit**: Provide visual charts and tables for usage trends and model distribution at `GET /`.

## 4. Maintenance: Automatic Log Retention
- **Current State**: `retention_days` is defined in config but not yet enforced.
- **Goal**: Implement a background worker to periodically delete records older than the retention limit.
- **Benefit**: Prevents the SQLite database from growing indefinitely.

## 5. Security: Proxy Authentication
- **Goal**: Add an optional `listen_api_key` for the ai-menshen service itself.
- **Benefit**: Enables secure deployment on private servers for small teams.

## 6. Data Portability: Export Options
- **Goal**: Support `?format=csv` for the report endpoint.
- **Benefit**: Allows users to easily export auditing data for external analysis or financial accounting.
