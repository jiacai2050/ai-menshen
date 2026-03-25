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
	db     *sql.DB
	queue  chan exchangeTask
	closed chan struct{}
}

type exchangeTask struct {
	request  RequestLog
	response ResponseLog
	usage    *UsageLog
}

func OpenStorage(path string) (*Storage, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// WAL mode for better concurrency
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	storage := &Storage{
		db:     db,
		queue:  make(chan exchangeTask, 1024),
		closed: make(chan struct{}),
	}
	if err := storage.init(); err != nil {
		db.Close()
		return nil, err
	}

	go storage.worker()

	return storage, nil
}

func (s *Storage) init() error {
	statements := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA busy_timeout = 5000;`,
		// 1. Metadata tables
		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			model TEXT NOT NULL,
			path TEXT NOT NULL,
			cache_key TEXT,
			created_at INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS response_logs (
			request_id TEXT PRIMARY KEY,
			status_code INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			from_cache INTEGER DEFAULT 0,
			cache_hit_request_id TEXT,
			FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
		);`,
		// 2. Dedicated body tables
		`CREATE TABLE IF NOT EXISTS request_bodies (
			request_id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS response_bodies (
			request_id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
		);`,
		// 3. Usage table
		`CREATE TABLE IF NOT EXISTS usage_logs (
			request_id TEXT PRIMARY KEY,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			total_tokens INTEGER,
			cached_tokens INTEGER,
			FOREIGN KEY(request_id) REFERENCES request_logs(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_created_at ON request_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_cache_key ON request_logs(cache_key);`,
	}

	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}
	return nil
}

func (s *Storage) SaveExchange(request RequestLog, response ResponseLog, usage *UsageLog) error {
	select {
	case s.queue <- exchangeTask{request, response, usage}:
		return nil
	default:
		return fmt.Errorf("storage queue full, dropping log for %s", request.ID)
	}
}

func (s *Storage) worker() {
	defer close(s.closed)
	for task := range s.queue {
		if err := s.saveExchangeSync(task.request, task.response, task.usage); err != nil {
			fmt.Fprintf(os.Stderr, "sqlite worker error: %v\n", err)
		}
	}
}

func (s *Storage) saveExchangeSync(request RequestLog, response ResponseLog, usage *UsageLog) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite transaction: %w", err)
	}
	defer tx.Rollback()

	// 1. Metadata
	if _, err := tx.Exec(
		`INSERT INTO request_logs (id, model, path, cache_key, created_at) VALUES (?, ?, ?, ?, ?)`,
		request.ID,
		request.Model,
		request.Path,
		nullIfEmpty(request.CacheKey),
		request.CreatedAt.UnixMilli(),
	); err != nil {
		return fmt.Errorf("insert request log: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO response_logs (request_id, status_code, duration_ms, from_cache, cache_hit_request_id) VALUES (?, ?, ?, ?, ?)`,
		response.RequestID,
		response.StatusCode,
		response.DurationMS,
		boolToInt(response.FromCache),
		nullIfEmpty(response.CacheHitRequestID),
	); err != nil {
		return fmt.Errorf("insert response log: %w", err)
	}

	// 2. Bodies
	if request.RequestBody != "" {
		if _, err := tx.Exec(`INSERT INTO request_bodies (request_id, content) VALUES (?, ?)`, request.ID, request.RequestBody); err != nil {
			return fmt.Errorf("insert request body: %w", err)
		}
	}
	if response.ResponseBody != "" {
		if _, err := tx.Exec(`INSERT INTO response_bodies (request_id, content) VALUES (?, ?)`, response.RequestID, response.ResponseBody); err != nil {
			return fmt.Errorf("insert response body: %w", err)
		}
	}

	// 3. Usage
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
		SELECT rl.id, rs.status_code, rb.content
		FROM request_logs rl
		JOIN response_logs rs ON rs.request_id = rl.id
		JOIN response_bodies rb ON rb.request_id = rl.id
		WHERE rl.cache_key = ?
		  AND rs.status_code = 200
		  AND rs.from_cache = 0
	`

	args := []any{cacheKey}
	if maxBodyBytes > 0 {
		query += ` AND LENGTH(rb.content) <= ?`
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

func (s *Storage) RequestLogs(days int, limit int) ([]LogEntry, error) {
	rows, err := s.db.Query(`
		SELECT 
			rl.id, rl.created_at, rl.model, rl.path, 
			rs.status_code, rs.duration_ms, 
			COALESCE(ul.total_tokens, 0),
			COALESCE(SUBSTR(rb.content, 1, 200), ''),
			COALESCE(SUBSTR(rsb.content, 1, 200), '')
		FROM request_logs rl
		JOIN response_logs rs ON rs.request_id = rl.id
		LEFT JOIN usage_logs ul ON ul.request_id = rl.id
		LEFT JOIN request_bodies rb ON rb.request_id = rl.id
		LEFT JOIN response_bodies rsb ON rsb.request_id = rl.id
		WHERE rl.created_at >= (unixepoch('now', ?) * 1000)
		ORDER BY rl.created_at DESC
		LIMIT ?
	`, fmt.Sprintf("-%d days", days), limit)
	if err != nil {
		return nil, fmt.Errorf("query request logs: %w", err)
	}
	defer rows.Close()

	var logs []LogEntry
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(
			&l.ID, &l.CreatedAt, &l.Model, &l.Path,
			&l.StatusCode, &l.DurationMS, &l.TotalTokens,
			&l.RequestBodyPreview, &l.ResponseBodyPreview,
		); err != nil {
			return nil, fmt.Errorf("scan log entry: %w", err)
		}
		logs = append(logs, l)
	}
	return logs, nil
}

func (s *Storage) RequestDetail(id string) (*LogDetail, error) {
	var d LogDetail
	err := s.db.QueryRow(`
		SELECT 
			rl.id, rl.created_at, rl.model, rl.path, 
			rs.status_code, rs.duration_ms, 
			COALESCE(rsb.content, ''),
			COALESCE(rb.content, ''),
			COALESCE(ul.total_tokens, 0)
		FROM request_logs rl
		JOIN response_logs rs ON rs.request_id = rl.id
		LEFT JOIN usage_logs ul ON ul.request_id = rl.id
		LEFT JOIN request_bodies rb ON rb.request_id = rl.id
		LEFT JOIN response_bodies rsb ON rsb.request_id = rl.id
		WHERE rl.id = ?
	`, id).Scan(
		&d.ID, &d.CreatedAt, &d.Model, &d.Path,
		&d.StatusCode, &d.DurationMS, &d.ResponseBody,
		&d.RequestBody,
		&d.TotalTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("query request detail: %w", err)
	}
	return &d, nil
}

func (s *Storage) UsageSummary(days int) (*UsageSummary, error) {
	var summary UsageSummary
	err := s.db.QueryRow(`
		SELECT
			COUNT(*) AS total_requests,
			SUM(CASE WHEN rs.from_cache = 1 THEN 1 ELSE 0 END) AS cache_hits,
			SUM(COALESCE(ul.total_tokens, 0)) AS total_tokens,
			SUM(COALESCE(ul.prompt_tokens, 0)) AS prompt_tokens,
			SUM(COALESCE(ul.completion_tokens, 0)) AS completion_tokens,
			SUM(COALESCE(ul.cached_tokens, 0)) AS cached_tokens
		FROM request_logs rl
		JOIN response_logs rs ON rs.request_id = rl.id
		LEFT JOIN usage_logs ul ON ul.request_id = rl.id
		WHERE rl.created_at >= (unixepoch('now', ?) * 1000)
	`, fmt.Sprintf("-%d days", days)).Scan(
		&summary.TotalRequests,
		&summary.CacheHits,
		&summary.TotalTokens,
		&summary.PromptTokens,
		&summary.CompletionTokens,
		&summary.CachedTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("query usage summary: %w", err)
	}
	return &summary, nil
}

func (s *Storage) DailyUsage(days int) ([]DailyUsage, error) {
	rows, err := s.db.Query(`
		SELECT
			DATE(rl.created_at / 1000, 'unixepoch') AS date,
			SUM(COALESCE(ul.total_tokens, 0)) AS total_tokens,
			SUM(COALESCE(ul.prompt_tokens, 0)) AS prompt_tokens,
			SUM(COALESCE(ul.completion_tokens, 0)) AS completion_tokens,
			COUNT(*) AS request_count
		FROM request_logs rl
		LEFT JOIN usage_logs ul ON ul.request_id = rl.id
		WHERE rl.created_at >= (unixepoch('now', ?) * 1000)
		GROUP BY date
		ORDER BY date ASC
	`, fmt.Sprintf("-%d days", days))
	if err != nil {
		return nil, fmt.Errorf("query daily usage: %w", err)
	}
	defer rows.Close()

	var results []DailyUsage
	for rows.Next() {
		var d DailyUsage
		if err := rows.Scan(&d.Date, &d.TotalTokens, &d.PromptTokens, &d.CompletionTokens, &d.RequestCount); err != nil {
			return nil, fmt.Errorf("scan daily usage: %w", err)
		}
		results = append(results, d)
	}
	return results, nil
}

func (s *Storage) ModelUsageReports(days int) ([]ModelUsageReport, error) {
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
		WHERE rl.created_at >= (unixepoch('now', ?) * 1000)
		GROUP BY rl.model
		ORDER BY total_tokens DESC, request_count DESC
	`, fmt.Sprintf("-%d days", days))
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

func (s *Storage) Close() error {
	close(s.queue)
	<-s.closed
	return s.db.Close()
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
