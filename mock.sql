-- ai-menshen Mock Data Generator (Robust Time Edition)
-- This script generates 3000 records using a simpler timestamp calculation.

DELETE FROM usage_logs;
DELETE FROM response_logs;
DELETE FROM request_logs;

-- 1. Generate request logs
-- (unixepoch('now') - days_offset) * 1000
INSERT INTO request_logs (id, created_at, path, model, cache_key, request_body)
WITH RECURSIVE cnt(x) AS (
    SELECT 1 UNION ALL SELECT x + 1 FROM cnt WHERE x < 3000
)
SELECT 
    'mock-req-' || x || '-' || lower(hex(randomblob(4))),
    (unixepoch('now') - (x % 30) * 86400 - (abs(random()) % 86400)) * 1000,
    '/chat/completions',
    CASE (x % 3)
        WHEN 0 THEN 'gpt-4o'
        WHEN 1 THEN 'gpt-4.1-mini'
        ELSE 'claude-3-5-sonnet'
    END,
    'cache-key-' || (x % 500),
    CASE (x % 3)
        WHEN 0 THEN '{"model":"gpt-4o","messages":[{"role":"user","content":"How to write a Go proxy?"}],"stream":false}'
        WHEN 1 THEN '{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"Tell me a joke about AI."}],"stream":true}'
        ELSE '{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"Analyze the current market trends."}],"stream":false}'
    END
FROM cnt;

-- 2. Generate response logs
INSERT INTO response_logs (request_id, status_code, duration_ms, from_cache, response_body)
SELECT 
    id,
    200,
    (abs(random()) % 2000) + 100,
    CASE WHEN (abs(random()) % 4) = 0 THEN 1 ELSE 0 END,
    CASE model
        WHEN 'gpt-4o' THEN '{"id":"chatcmpl-123","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"To write a Go proxy, you can use the net/http package and reverse proxy logic..."},"finish_reason":"stop"}]}'
        WHEN 'gpt-4.1-mini' THEN '{"id":"chatcmpl-456","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Why did the AI cross the road? To optimize the path to the other side!"},"finish_reason":"stop"}]}'
        ELSE '{"id":"msg-789","type":"message","role":"assistant","content":"The current market trends show a significant shift towards local-first AI solutions..."}'
    END
FROM request_logs;

-- 3. Generate usage logs
INSERT INTO usage_logs (request_id, prompt_tokens, completion_tokens, total_tokens, cached_tokens)
SELECT 
    id,
    (abs(random()) % 1500) + 50,
    (abs(random()) % 1000) + 20,
    0,
    CASE WHEN from_cache = 1 THEN (abs(random()) % 200) ELSE 0 END
FROM response_logs rs
JOIN request_logs rl ON rl.id = rs.request_id;

UPDATE usage_logs SET total_tokens = prompt_tokens + completion_tokens;

SELECT 'Successfully generated 3000 mock records with robust timestamps!' as status;
