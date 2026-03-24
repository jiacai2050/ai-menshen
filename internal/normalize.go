package aimenshen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func AnalyzeRequest(path string, body []byte, provider ProviderConfig) (RequestMeta, error) {
	meta := RequestMeta{
		EffectiveBody: body,
	}

	if len(body) == 0 {
		return meta, nil
	}

	payload, ok := decodeJSONObject(body)
	if !ok {
		return meta, nil
	}

	needsMarshal := false
	if provider.Model != "" {
		if current, ok := payload["model"].(string); !ok || current != provider.Model {
			payload["model"] = provider.Model
			needsMarshal = true
		}
	}

	if model, ok := payload["model"].(string); ok {
		meta.EffectiveModel = model
	}

	if stream, ok := payload["stream"].(bool); ok {
		meta.Stream = stream
	}

	if needsMarshal {
		effectiveBody, err := json.Marshal(payload)
		if err != nil {
			return meta, fmt.Errorf("marshal effective request body: %w", err)
		}
		meta.EffectiveBody = effectiveBody
	}

	if meta.Stream {
		return meta, nil
	}

	cacheKey, err := buildCacheKey(path, payload)
	if err != nil {
		return meta, fmt.Errorf("build cache key: %w", err)
	}
	meta.CacheKey = cacheKey

	return meta, nil
}

func decodeJSONObject(body []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false
	}

	object, ok := payload.(map[string]any)
	return object, ok
}

func buildCacheKey(path string, payload map[string]any) (string, error) {
	normalized := map[string]any{
		"path":    path,
		"request": normalizeForCache(payload),
	}

	encoded, err := marshalCanonical(normalized)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeForCache(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			if shouldExcludeCacheField(key, item) {
				continue
			}
			normalized[key] = normalizeForCache(item)
		}
		return normalized
	case []any:
		items := make([]any, len(typed))
		for i, item := range typed {
			items[i] = normalizeForCache(item)
		}
		return items
	default:
		return typed
	}
}

func shouldExcludeCacheField(key string, value any) bool {
	switch key {
	case "user", "stream_options":
		return true
	case "stream":
		stream, ok := value.(bool)
		return ok && !stream
	default:
		return false
	}
}

func marshalCanonical(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeCanonicalJSON(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeCanonicalJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case json.Number:
		buffer.WriteString(typed.String())
	case float64:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		buffer.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buffer.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			if err := writeCanonicalJSON(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []any:
		buffer.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				buffer.WriteByte(',')
			}
			if err := writeCanonicalJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	}

	return nil
}
