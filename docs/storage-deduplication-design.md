# Storage Deduplication & Blob Separation Design

This document outlines the architecture for optimizing SQLite storage in **ai-menshen** by separating large text blobs from metadata and deduplicating them using content hashing and **Reference Counting**.

## 1. Motivation
In an AI proxy, the same Large Language Model (LLM) responses are often repeated (Cache Hits). Storing these multi-kilobyte strings for every single request leads to disk bloat and slow metadata queries.

## 2. Core Schema

### 2.1 The `blobs` Table (Content-Addressable)
A deduplicated store for all request and response bodies.

| Column | Type | Description |
| :--- | :--- | :--- |
| `hash` | `TEXT (PK)` | SHA-256 hex hash of the content. |
| `content` | `TEXT` | The actual JSON string or body content. |
| `ref_count` | `INTEGER` | Number of logs currently referencing this blob. Defaults to 0. |

### 2.2 Metadata Tables
`request_logs` and `response_logs` store references to `blobs.hash`.

```sql
CREATE TABLE IF NOT EXISTS blobs (
    hash TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    ref_count INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS request_logs (
    id TEXT PRIMARY KEY,
    model TEXT NOT NULL,
    path TEXT NOT NULL,
    cache_key TEXT,
    request_body_hash TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS response_logs (
    request_id TEXT PRIMARY KEY, 
    status_code INTEGER NOT NULL,
    response_body_hash TEXT,
    duration_ms INTEGER NOT NULL,
    from_cache INTEGER DEFAULT 0,
    cache_hit_request_id TEXT,
    FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
);
```

---

## 3. Reference Counting Strategies

We have identified two ways to maintain the `ref_count` and perform garbage collection.

### Strategy A: Database Triggers (Automated & Atomic)

Logic is implemented directly in SQLite using triggers. This approach provides strong consistency and real-time reclamation.

#### 3.1 Incrementing Reference Count (On Insert)
```sql
CREATE TRIGGER IF NOT EXISTS trg_request_ref_inc
AFTER INSERT ON request_logs
BEGIN
    UPDATE blobs SET ref_count = ref_count + 1 WHERE hash = NEW.request_body_hash;
END;

CREATE TRIGGER IF NOT EXISTS trg_response_ref_inc
AFTER INSERT ON response_logs
BEGIN
    UPDATE blobs SET ref_count = ref_count + 1 WHERE hash = NEW.response_body_hash;
END;
```

#### 3.2 Decrementing and Cleanup (On Delete)
```sql
CREATE TRIGGER IF NOT EXISTS trg_request_ref_dec
AFTER DELETE ON request_logs
BEGIN
    UPDATE blobs SET ref_count = ref_count - 1 WHERE hash = OLD.request_body_hash;
    DELETE FROM blobs WHERE hash = OLD.request_body_hash AND ref_count <= 0;
END;

CREATE TRIGGER IF NOT EXISTS trg_response_ref_dec
AFTER DELETE ON response_logs
BEGIN
    UPDATE blobs SET ref_count = ref_count - 1 WHERE hash = OLD.response_body_hash;
    DELETE FROM blobs WHERE hash = OLD.response_body_hash AND ref_count <= 0;
END;
```

### Strategy B: Application Worker (Explicit & Traceable)
Logic is implemented in the Go backend.
- **On Insert**: The background worker explicitly updates `ref_count` after saving the log entry.
- **On Cleanup**: A periodic task (e.g., every hour) identifies orphans and deletes them.
- **Best for**: Debuggability, avoiding "magic" database behavior, and easier Schema migrations.

---

## 4. Operational Workflow (Common)

### 4.1 Writing
1.  **Calculate Hash**: Generate SHA-256 of the body content.
2.  **Upsert Blob**: `INSERT OR IGNORE INTO blobs (hash, content, ref_count) VALUES (?, ?, 0)`.
3.  **Link Metadata**: Save the log entry referencing the hash. Strategy A or B will handle the `ref_count` accordingly.

### 4.2 Retrieval
Standard `JOIN` operations fetch content only when needed (e.g., for detailed log inspection or cache replay).

## 5. Key Advantages
1.  **Storage Efficiency**: Near-zero growth for duplicate payloads (Cache Hits).
2.  **Metadata Performance**: Log tables remain slim and fast for filtering and trends.
3.  **Lifecycle Management**: `ref_count` provides a clear, scalable way to track blob ownership across multiple logs.

## 6. Maintenance
To reclaim physical disk space after blobs are deleted, it is recommended to occasionally run:
```sql
PRAGMA incremental_vacuum;
```
(Requires `PRAGMA auto_vacuum = INCREMENTAL;` set during database creation).
