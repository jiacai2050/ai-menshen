# Simple Blob Separation Design (Ultimate Slim Version)

This document describes the most efficient way to optimize SQLite storage by separating large text blobs into dedicated tables using existing identifiers.

## 1. Motivation
- **Maximum Table Thinning**: Metadata tables (`request_logs`, `response_logs`) are reduced to their absolute minimal size, ensuring lightning-fast dashboard queries even with millions of records.
- **Zero Redundancy**: No extra "Body IDs" or "Hashes" are stored. We reuse the existing `request_id`.
- **Automatic Cleanup**: Leveraging SQLite's `ON DELETE CASCADE` to manage the lifecycle of large blobs without any background workers or complex logic.

## 2. Schema Definition

### 2.1 Metadata Tables (Slim)
These tables contain only indexed scalars and small strings.

```sql
CREATE TABLE IF NOT EXISTS request_logs (
    id TEXT PRIMARY KEY,
    model TEXT NOT NULL,
    path TEXT NOT NULL,
    cache_key TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS response_logs (
    request_id TEXT PRIMARY KEY, 
    status_code INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    from_cache INTEGER DEFAULT 0,
    cache_hit_request_id TEXT,
    FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
);
```

### 2.2 Body Tables (Dedicated)
Large text blobs are stored in separate tables indexed by the same `request_id`.

```sql
CREATE TABLE IF NOT EXISTS request_bodies (
    request_id TEXT PRIMARY KEY, 
    content TEXT NOT NULL,
    FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS response_bodies (
    request_id TEXT PRIMARY KEY, 
    content TEXT NOT NULL,
    FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
);
```

## 3. Operational Workflow

### 3.1 Writing
1.  Insert metadata into `request_logs`.
2.  Insert the raw request JSON into `request_bodies` using the same `id`.
3.  Upon response, insert metadata into `response_logs` and the JSON into `response_bodies`.

### 3.2 Reading
- **Dashboard Trends**: `SELECT model, created_at FROM request_logs` (Minimal IO).
- **Log Inspection**: `SELECT content FROM request_bodies WHERE request_id = ?`.
- **Cache Lookup**: `SELECT content FROM response_bodies WHERE request_id = (SELECT request_id FROM request_logs WHERE cache_key = ? LIMIT 1)`.

## 4. Maintenance (Zero Effort)
Because of the `ON DELETE CASCADE` constraint, deleting a log entry automatically and atomically purges the associated request and response bodies. No orphan cleanup or manual "Garbage Collection" is required.

## 5. Summary of Benefits
- **CPU**: Zero extra cost (No hashing).
- **Storage**: Clean 1-to-1 mapping.
- **Performance**: Metadata tables are physically small, maximizing SQLite's Page Cache efficiency.
- **Simplicity**: No extra columns, no extra IDs, no extra background tasks.
