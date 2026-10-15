package codexapp

import "encoding/json"

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
