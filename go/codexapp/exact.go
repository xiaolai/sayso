package codexapp

import (
	"encoding/json"
	"strings"
	"unsafe"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
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

// The readers below read a validated document where it lies (wire.Member), for the lines and results that may hold
// a credential: nothing of the document is copied but the strings they return.

// present is the member name of the object at obj, unless it is absent or null.
func present(doc []byte, obj wire.Span, name string) (wire.Span, bool) {
	v, ok := wire.Member(doc, obj, name)
	return v, ok && wire.KindOf(doc, v) != 'n'
}

// decoded is the string at v, decoded into a buffer that is cleared once the string returned is made from it: the
// string is the one copy left.
func decoded(doc []byte, v wire.Span) string {
	buf := make([]byte, 0, v.End-v.Start)
	defer clear(buf[:cap(buf)])
	buf, _ = wire.AppendString(buf, doc, v)
	return string(buf)
}

// scrubbed is the string at v, decoded and scrubbed. The scrubber reads the decoded bytes in place, and they are
// cleared, so only the scrubbed text is left: a secret it removes leaves no copy behind.
func scrubbed(doc []byte, v wire.Span) string {
	buf := make([]byte, 0, v.End-v.Start)
	defer clear(buf[:cap(buf)])
	buf, _ = wire.AppendString(buf, doc, v)
	return strings.Clone(secret.Scrub(unsafe.String(unsafe.SliceData(buf), len(buf)))) // never the view itself
}
