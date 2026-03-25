-- ai-menshen Mock Data Generator (Ultimate Slim Version)
-- Matching the schema: request_logs, response_logs, request_bodies, response_bodies, usage_logs

-- 1. Clear existing data
DELETE FROM usage_logs;
DELETE FROM response_bodies;
DELETE FROM request_bodies;
DELETE FROM response_logs;
DELETE FROM request_logs;

-- 2. Generate 1000 requests spread over the last 30 days
INSERT INTO request_logs (id, created_at, path, model, cache_key)
WITH RECURSIVE cnt(x) AS (
    SELECT 1 UNION ALL SELECT x + 1 FROM cnt WHERE x < 1000
)
SELECT 
    'mock-req-' || x || '-' || lower(hex(randomblob(4))),
    (unixepoch('now') - (x % 30) * 86400 - (abs(random()) % 86400)) * 1000,
    '/v1/chat/completions',
    CASE (x % 3)
        WHEN 0 THEN 'gpt-4o'
        WHEN 1 THEN 'gpt-4.1-mini'
        ELSE 'claude-3-5-sonnet'
    END,
    'cache-key-' || (x % 200)
FROM cnt;

-- 3. Generate request bodies (1-to-1 with request_logs)
INSERT INTO request_bodies (request_id, content)
SELECT 
    id,
    '{"model":"' || model || '","messages":[{"role":"user","content":"Mock question for ' || id || '"}],"stream":false}'
FROM request_logs;

-- 4. Generate response logs
INSERT INTO response_logs (request_id, status_code, duration_ms, from_cache)
SELECT 
    id,
    200,
    (abs(random()) % 2000) + 100,
    CASE WHEN (abs(random()) % 5) = 0 THEN 1 ELSE 0 END
FROM request_logs;

-- 5. Generate response bodies (1-to-1 with response_logs)
INSERT INTO response_bodies (request_id, content)
SELECT 
    id,
    '{"id":"chatcmpl-' || lower(hex(randomblob(8))) || '","choices":[{"index":0,"message":{"role":"assistant","content":"Mock response for ' || id || '"},"finish_reason":"stop"}]}'
FROM request_logs;

-- 6. Generate usage logs
INSERT INTO usage_logs (request_id, prompt_tokens, completion_tokens, total_tokens, cached_tokens)
SELECT 
    id,
    (abs(random()) % 1500) + 50,
    (abs(random()) % 1000) + 20,
    0,
    CASE WHEN (SELECT from_cache FROM response_logs WHERE request_id = id) = 1 THEN (abs(random()) % 200) ELSE 0 END
FROM request_logs;

UPDATE usage_logs SET total_tokens = prompt_tokens + completion_tokens;

SELECT 'Successfully generated 1000 mock records for the slim schema!' as status;
