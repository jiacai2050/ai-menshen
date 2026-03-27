# Future Plans & Ideas

This document outlines potential features and improvements for **ai-menshen**.

## 1. Reliability: Multi-Provider Failover
- **Current State**: Only `providers[0]` is utilized.
- **Goal**: Implement automatic failover if the primary provider returns 5xx errors or timeouts.
- **Benefit**: High availability for users relying on multiple API upstream channels.

## 2. Cost Management: Token-to-Price Mapping
- **Goal**: Add optional pricing configuration (e.g., price per 1k tokens) for specific models.
- **Benefit**: Include `estimated_cost` in the usage reports (`GET /__report/models`) for easier budgeting.

## 3. Data Portability: Export Options
- **Goal**: Support `?format=csv` for the report endpoint.
- **Benefit**: Allows users to easily export auditing data for external analysis or financial accounting.
