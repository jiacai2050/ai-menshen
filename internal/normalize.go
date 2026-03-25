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

	meta.CacheKey = buildCacheKey(path, payload)

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

func buildCacheKey(path string, payload map[string]any) string {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	buf.WriteString(`{"path":`)
	encodeString(buf, path)
	buf.WriteString(`,"request":`)
	writeCanonicalJSON(buf, payload, true)
	buf.WriteByte('}')

	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	defer sha256Pool.Put(h)

	h.Write(buf.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

func writeCanonicalJSON(w io.Writer, value any, isRootRequest bool) {
	switch typed := value.(type) {
	case nil:
		io.WriteString(w, "null")
	case bool:
		if typed {
			io.WriteString(w, "true")
		} else {
			io.WriteString(w, "false")
		}
	case string:
		encodeString(w, typed)
	case json.Number:
		io.WriteString(w, typed.String())
	case float64:
		io.WriteString(w, strconv.FormatFloat(typed, 'f', -1, 64))
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			if isRootRequest && shouldExcludeCacheField(k, typed[k]) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)

		io.WriteString(w, "{")
		for i, k := range keys {
			if i > 0 {
				io.WriteString(w, ",")
			}
			encodeString(w, k)
			io.WriteString(w, ":")
			writeCanonicalJSON(w, typed[k], false)
		}
		io.WriteString(w, "}")
	case []any:
		io.WriteString(w, "[")
		for i, v := range typed {
			if i > 0 {
				io.WriteString(w, ",")
			}
			writeCanonicalJSON(w, v, false)
		}
		io.WriteString(w, "]")
	default:
		b, _ := json.Marshal(typed)
		w.Write(b)
	}
}

func encodeString(w io.Writer, s string) {
	b, _ := json.Marshal(s)
	w.Write(b)
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
