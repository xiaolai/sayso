package codexapp

import (
	"encoding/json"

	"github.com/xiaolai/sayso/go/internal/wire"
)

// MaxJSONDepth is how deeply Codex's JSON (a reply, auth.json, a JWT payload) may nest: a bound both implementations'
// decoders accept (Foundation's stops at 512), so a deeper document is malformed in both rather than in one.
const MaxJSONDepth = wire.MaxCodexDepth

// ValidJSON reports whether b is JSON as Codex's is read (spec/credentials.md, messages): one strict value nested at
// most MaxJSONDepth deep, with no number too large for a float64. It copies nothing of b.
func ValidJSON(b []byte) bool { return wire.ValidCodex(b) }

// Replies are read with exact key names. encoding/json matches struct fields case-insensitively, so a reply with
// "AUTHTOKEN" would be read here and ignored by the Swift implementation; a map lookup is exact in both.

// object decodes raw, a JSON object or null (an empty object), with exact keys.
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// optional returns key of the object raw decoded as T; nil when the key is absent or null. A value of another type
// is an error.
func optional[T any](raw json.RawMessage, key string) (*T, error) {
	m, err := object(raw)
	if err != nil {
		return nil, err
	}
	v, ok := m[key]
	if !ok || string(v) == "null" {
		return nil, nil
	}
	var out T
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// field is optional with the zero value for an absent or null key.
func field[T any](raw json.RawMessage, key string) (T, error) {
	var zero T
	p, err := optional[T](raw, key)
	if err != nil || p == nil {
		return zero, err
	}
	return *p, nil
}
