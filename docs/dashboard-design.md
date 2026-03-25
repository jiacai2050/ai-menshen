# Dashboard Technical Design

This document outlines the technical selection and architecture for the built-in visualization dashboard in **ai-menshen**.

## 1. Design Philosophy
- **Local-First**: No external CDNs. All assets (JS/CSS) must be embedded in the Go binary.
- **Zero-Dependency Runtime**: The dashboard should be a single-page application (SPA) that requires no node_modules or complex build steps for the end-user.
- **High Information Density**: Focus on "at-a-glance" metrics: costs, savings, and performance.
- **Privacy by Default**: Data never leaves the local machine.

## 2. Technology Stack

### Frontend: Minimal & Robust
| Layer | Choice | Rationale |
| :--- | :--- | :--- |
| **Logic/Reactivity** | **Alpine.js** | Extremely lightweight (approx. 15KB gzipped). Provides Vue-like reactivity with zero build step. Perfect for simple data binding from APIs. |
| **Styling** | **Pico.css** | A "minimalist" CSS framework. It styles native HTML elements automatically. No need for thousands of utility classes, keeping the HTML clean and small. |
| **Charting** | **Chart.js** | The industry standard for canvas-based charts. Supports responsive layouts and beautiful animations with a small footprint. |
| **Icons** | **Lucide-static** | Simple, consistent SVG icons. We will only embed the few icons we actually use. |

### Backend: Standard Go
| Component | Choice | Description |
| :--- | :--- | :--- |
| **Static Assets** | `embed` | Use Go 1.16+ `embed` package to bundle the frontend into the binary. |
| **API Layer** | REST/JSON | Extend existing `internal/proxy.go` with new `/___report/` endpoints. |
| **Data Aggregation** | SQLite SQL | Leverage SQLite's powerful aggregation functions (`strftime`, `SUM`, `COUNT`) to keep the Go logic thin. |

## 3. Architecture & Data Flow

### Directory Structure
```text
internal/
  ├── web/
  │    ├── assets/
  │    │    ├── chart.min.js
  │    │    ├── alpine.min.js
  │    │    └── pico.min.css
  │    ├── index.html       # Single SPA file
  │    └── web.go           # //go:embed assets/* index.html
  └── proxy.go              # Routes GET / to serve index.html
```

### Key Metrics to Visualize
1. **The "Big Three" Stats**:
   - **Total Tokens**: Prompt vs. Completion.
   - **Cache Hit Rate**: Percentage of requests served from local SQLite.
   - **Estimated Savings**: Dollars saved via cache (based on configurable model pricing).
2. **Daily Activity**: A bar chart showing token consumption over the last 14 days.
3. **Model Mix**: A doughnut chart showing which models are being used most.
4. **Latency Heatmap**: Average response time per model to identify slow upstreams.

## 4. Implementation Roadmap

### Phase 1: Storage Expansion
- Update `internal/storage.go` to provide time-series data:
  - `QueryDailyUsage(days int)`
  - `QueryModelBreakdown()`

### Phase 2: Frontend Scaffolding
- Set up `internal/web` with `index.html` using Pico.css.
- Implement basic "Total Stats" cards using Alpine.js fetching JSON from `/___report/summary`.

### Phase 3: Chart Integration
- Integrate Chart.js to render the Daily Activity and Model Mix.
- Optimize the UI for both Light and Dark modes (Pico.css supports this natively).

---
*Status: Draft Proposal. Ready for Phase 1 implementation upon request.*
