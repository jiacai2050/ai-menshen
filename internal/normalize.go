package aimenshen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"
)

var (
	bufferPool = sync.Pool{
		New: func() any { return new(bytes.Buffer) },
	}
)

// requestFields is a decoded top-level JSON object. Each field also keeps its
// raw value bytes (a subslice of the input body) so a patched request body can
// reuse untouched fields verbatim instead of re-encoding them.
type requestFields struct {
	values map[string]any
	raw    map[string][]byte
}

// AnalyzeRequest inspects a request body and only rewrites it when a provider
// model override or injected stream options require it. Rewrites splice the
// raw bytes of untouched top-level fields, so large payloads (messages, tools)
// are never re-encoded.
func AnalyzeRequest(path string, body []byte, provider ProviderConfig, cacheEnabled bool) (RequestMeta, error) {
	meta := RequestMeta{
		EffectiveBody: body,
	}

	if len(body) == 0 {
		return meta, nil
	}

	fields, ok := decodeJSONObject(body)
	if !ok {
		return meta, nil
	}

	overrides := map[string][]byte(nil)
	patch := func(key string, value any, raw []byte) {
		fields.values[key] = value
		if overrides == nil {
			overrides = make(map[string][]byte, 2)
		}
		overrides[key] = raw
	}

	if provider.Model != "" {
		if current, ok := fields.values["model"].(string); !ok || current != provider.Model {
			raw, err := json.Marshal(provider.Model)
			if err != nil {
				return meta, fmt.Errorf("marshal provider model: %w", err)
			}
			patch("model", provider.Model, raw)
		}
	}

	if model, ok := fields.values["model"].(string); ok {
		meta.EffectiveModel = model
	}

	if stream, ok := fields.values["stream"].(bool); ok {
		meta.Stream = stream
	}

	if meta.Stream {
		if _, exists := fields.values["stream_options"]; !exists {
			patch("stream_options", map[string]any{"include_usage": true}, []byte(`{"include_usage":true}`))
		}
	}

	if overrides != nil {
		effectiveBody, err := buildEffectiveBody(fields, overrides)
		if err != nil {
			return meta, fmt.Errorf("marshal effective request body: %w", err)
		}
		meta.EffectiveBody = effectiveBody
	}

	if cacheEnabled {
		cacheKey, err := buildCacheKey(path, fields.values)
		if err != nil {
			return meta, fmt.Errorf("build cache key: %w", err)
		}
		meta.CacheKey = cacheKey
	}

	return meta, nil
}

// decodeJSONObject decodes the top level of a JSON object in a single pass,
// capturing the raw bytes of each value alongside its decoded form.
func decodeJSONObject(body []byte) (requestFields, bool) {
	fields := requestFields{
		values: make(map[string]any),
		raw:    make(map[string][]byte),
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return requestFields{}, false
	}

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return requestFields{}, false
		}
		key, ok := token.(string)
		if !ok {
			return requestFields{}, false
		}

		start := decoder.InputOffset()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return requestFields{}, false
		}
		end := decoder.InputOffset()

		fields.values[key] = value
		// The span starts at the ":" separating key and value; trimming it
		// leaves the value bytes verbatim.
		fields.raw[key] = bytes.TrimLeft(body[start:end], ": \t\r\n")
	}

	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return requestFields{}, false
	}
	return fields, true
}

// buildEffectiveBody re-encodes the top-level object with sorted keys, using
// each field's raw bytes (patched fields use their override).
func buildEffectiveBody(fields requestFields, overrides map[string][]byte) ([]byte, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := buf.WriteByte('{'); err != nil {
		return nil, err
	}
	for i, key := range sortedKeys(fields.values) {
		if i > 0 {
			if err := buf.WriteByte(','); err != nil {
				return nil, err
			}
		}
		if err := encodeString(buf, key); err != nil {
			return nil, err
		}
		if err := buf.WriteByte(':'); err != nil {
			return nil, err
		}
		raw, ok := overrides[key]
		if !ok {
			raw = fields.raw[key]
		}
		if _, err := buf.Write(raw); err != nil {
			return nil, err
		}
	}
	if err := buf.WriteByte('}'); err != nil {
		return nil, err
	}
	return bytes.Clone(buf.Bytes()), nil
}

const cacheVersion = "v1"

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
	if _, err := buf.WriteString(`,"request":{`); err != nil {
		return "", err
	}
	written := 0
	for _, key := range sortedKeys(payload) {
		if shouldExcludeCacheField(key, payload[key]) {
			continue
		}
		if written > 0 {
			if err := buf.WriteByte(','); err != nil {
				return "", err
			}
		}
		written++
		if err := encodeString(buf, key); err != nil {
			return "", err
		}
		if err := buf.WriteByte(':'); err != nil {
			return "", err
		}
		if err := writeCanonicalJSON(buf, payload[key]); err != nil {
			return "", err
		}
	}
	if _, err := buf.WriteString(`}}`); err != nil {
		return "", err
	}

	sum := sha256.Sum256(buf.Bytes())
	return cacheVersion + ":" + hex.EncodeToString(sum[:]), nil
}

func sortedKeys[V any](payload map[string]V) []string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func writeCanonicalJSON(w io.Writer, value any) error {
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
		if _, err := io.WriteString(w, "{"); err != nil {
			return err
		}
		for i, k := range sortedKeys(typed) {
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
			if err := writeCanonicalJSON(w, typed[k]); err != nil {
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
			if err := writeCanonicalJSON(w, v); err != nil {
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
