package aimenshen

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Storage struct {
	db *sql.DB
}

func OpenStorage(path string) (*Storage, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	storage := &Storage{db: db}
	if err := storage.init(); err != nil {
		db.Close()
		return nil, err
	}

	return storage, nil
}

func (s *Storage) Close() error {
	return s.db.Close()
}

func (s *Storage) init() error {
	statements := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA busy_timeout = 5000;`,
		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			path TEXT NOT NULL,
			model TEXT,
			cache_key TEXT,
			request_body TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS response_logs (
			request_id TEXT PRIMARY KEY,
			status_code INTEGER,
			response_body TEXT,
			duration_ms INTEGER NOT NULL,
			from_cache INTEGER NOT NULL DEFAULT 0,
			cache_hit_request_id TEXT,
			FOREIGN KEY(request_id) REFERENCES request_logs(id)
		);`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			request_id TEXT PRIMARY KEY,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			total_tokens INTEGER,
			cached_tokens INTEGER,
			FOREIGN KEY(request_id) REFERENCES request_logs(id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_cache_key ON request_logs(cache_key);`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);`,
	}

	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}

	return nil
}

func (s *Storage) SaveExchange(request RequestLog, response ResponseLog, usage *UsageLog) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO request_logs (id, created_at, path, model, cache_key, request_body) VALUES (?, ?, ?, ?, ?, ?)`,
		request.ID,
		request.CreatedAt.UTC().Format(time.RFC3339Nano),
		request.Path,
		nullIfEmpty(request.Model),
		nullIfEmpty(request.CacheKey),
		nullIfEmpty(request.RequestBody),
	); err != nil {
		return fmt.Errorf("insert request log: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO response_logs (request_id, status_code, response_body, duration_ms, from_cache, cache_hit_request_id) VALUES (?, ?, ?, ?, ?, ?)`,
		response.RequestID,
		response.StatusCode,
		nullIfEmpty(response.ResponseBody),
		response.DurationMS,
		boolToInt(response.FromCache),
		nullIfEmpty(response.CacheHitRequestID),
	); err != nil {
		return fmt.Errorf("insert response log: %w", err)
	}

	if usage != nil {
		if _, err := tx.Exec(
			`INSERT INTO usage_logs (request_id, prompt_tokens, completion_tokens, total_tokens, cached_tokens) VALUES (?, ?, ?, ?, ?)`,
			usage.RequestID,
			nullableInt64(usage.PromptTokens),
			nullableInt64(usage.CompletionTokens),
			nullableInt64(usage.TotalTokens),
			nullableInt64(usage.CachedTokens),
		); err != nil {
			return fmt.Errorf("insert usage log: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite transaction: %w", err)
	}

	return nil
}

func (s *Storage) FindCachedResponse(cacheKey string, maxBodyBytes int64) (*CachedResponse, error) {
	if cacheKey == "" {
		return nil, nil
	}

	query := `
		SELECT rl.id, rs.status_code, rs.response_body
		FROM request_logs rl
		JOIN response_logs rs ON rs.request_id = rl.id
		WHERE rl.cache_key = ?
		  AND rs.status_code = 200
		  AND rs.response_body IS NOT NULL
		  AND rs.response_body != ''
	`

	args := []any{cacheKey}
	if maxBodyBytes > 0 {
		query += ` AND LENGTH(rs.response_body) <= ?`
		args = append(args, maxBodyBytes)
	}

	query += ` ORDER BY rl.created_at DESC LIMIT 1`

	var cached CachedResponse
	err := s.db.QueryRow(query, args...).Scan(&cached.RequestID, &cached.StatusCode, &cached.ResponseBody)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query cached response: %w", err)
	}

	return &cached, nil
}

func (s *Storage) ModelUsageReports() ([]ModelUsageReport, error) {
	rows, err := s.db.Query(`
		SELECT
			COALESCE(NULLIF(rl.model, ''), '(unknown)') AS model,
			COUNT(*) AS request_count,
			SUM(CASE WHEN rs.from_cache = 1 THEN 1 ELSE 0 END) AS cache_hits,
			SUM(COALESCE(ul.prompt_tokens, 0)) AS prompt_tokens,
			SUM(COALESCE(ul.completion_tokens, 0)) AS completion_tokens,
			SUM(COALESCE(ul.total_tokens, 0)) AS total_tokens,
			SUM(COALESCE(ul.cached_tokens, 0)) AS cached_tokens
		FROM request_logs rl
		JOIN response_logs rs ON rs.request_id = rl.id
		LEFT JOIN usage_logs ul ON ul.request_id = rl.id
		GROUP BY rl.model
		ORDER BY total_tokens DESC, request_count DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query model usage report: %w", err)
	}
	defer rows.Close()

	reports := make([]ModelUsageReport, 0)
	for rows.Next() {
		var report ModelUsageReport
		if err := rows.Scan(
			&report.Model,
			&report.RequestCount,
			&report.CacheHits,
			&report.PromptTokens,
			&report.CompletionTokens,
			&report.TotalTokens,
			&report.CachedTokens,
		); err != nil {
			return nil, fmt.Errorf("scan model usage report: %w", err)
		}
		reports = append(reports, report)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model usage report: %w", err)
	}

	return reports, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
