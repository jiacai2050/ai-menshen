package aimenshen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"sync"
)

var (
	bufferPool = sync.Pool{
		New: func() any { return new(bytes.Buffer) },
	}
	sha256Pool = sync.Pool{
		New: func() any { return sha256.New() },
	}
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

	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false
	}

	object, ok := payload.(map[string]any)
	return object, ok
}

func buildCacheKey(path string, payload map[string]any) (string, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if _, err := buf.WriteString(`{"path":`); err != nil {
		return "", err
	}
	if err := encodeString(buf, path); err != nil {
		return "", err
	}
	if _, err := buf.WriteString(`,"request":`); err != nil {
		return "", err
	}
	if err := writeCanonicalJSON(buf, payload, true); err != nil {
		return "", err
	}
	if err := buf.WriteByte('}'); err != nil {
		return "", err
	}

	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	defer sha256Pool.Put(h)

	h.Write(buf.Bytes())
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeCanonicalJSON(w io.Writer, value any, isRootRequest bool) error {
	switch typed := value.(type) {
	case nil:
		_, err := io.WriteString(w, "null")
		return err
	case bool:
		var s string
		if typed {
			s = "true"
		} else {
			s = "false"
		}
		_, err := io.WriteString(w, s)
		return err
	case string:
		return encodeString(w, typed)
	case float64:
		_, err := io.WriteString(w, strconv.FormatFloat(typed, 'f', -1, 64))
		return err
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			if isRootRequest && shouldExcludeCacheField(k, typed[k]) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)

		if _, err := io.WriteString(w, "{"); err != nil {
			return err
		}
		for i, k := range keys {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := encodeString(w, k); err != nil {
				return err
			}
			if _, err := io.WriteString(w, ":"); err != nil {
				return err
			}
			if err := writeCanonicalJSON(w, typed[k], false); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "}")
		return err
	case []any:
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for i, v := range typed {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := writeCanonicalJSON(w, v, false); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]")
		return err
	default:
		b, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
}

func encodeString(w io.Writer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
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
