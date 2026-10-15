package codexapp

import (
	"encoding/json"

	"github.com/xiaolai/sayso/go/internal/wire"
)

// MaxJSONDepth is how deeply Codex's JSON (a reply, auth.json, a JWT payload) may nest. Codex's own documents nest a
// few levels; the bound is one both implementations' decoders accept (Foundation's stops at 512), so a deeper
// document is malformed in both rather than in one.
const MaxJSONDepth = 64

// ValidJSON reports whether b is JSON as Codex's is read (spec/credentials.md, messages): one strict value (valid
// UTF-8, no duplicate key; Codex's serde refuses both) nested at most MaxJSONDepth deep.
func ValidJSON(b []byte) bool { return depthWithin(b, MaxJSONDepth) && wire.Valid(b) }

// depthWithin reports whether no array or object in b nests deeper than max, counting outside strings only. It runs
// before the strict check, so it reads b as it is and never copies it.
func depthWithin(b []byte, max int) bool {
	depth, inString, escaped := 0, false, false
	for _, c := range b {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			if depth++; depth > max {
				return false
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return true
}

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
