// Package wire reads the JSON the provider sends with one strict rule set, which the Swift package implements too
// (spec/session.md, wire JSON): the whole document is one RFC 8259 value in valid UTF-8, with no duplicate member
// name in any object and at most 10,000 levels of nesting; member names match exactly; null is absence. Library
// decoders disagree on each of these (case-folded names, merged duplicates, replaced invalid UTF-8), which is why
// both implementations read the provider's JSON through this one grammar.
package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strconv"

	"golang.org/x/text/unicode/norm"
)

// ErrInvalid is any document or field the rules refuse.
var ErrInvalid = errors.New("wire: invalid JSON")

// Node is one JSON value: its exact bytes, as received.
type Node struct{ raw jsontext.Value }

// Valid reports whether data is one strict JSON value. Codex's own replies and files are read through it too, so a
// duplicate key or invalid UTF-8 is refused in Go as in Swift (Codex's serde refuses both).
func Valid(data []byte) bool {
	_, err := Parse(data)
	return err == nil
}

// Parse validates data as one strict JSON value, whitespace around it allowed, and returns its root.
func Parse(data []byte) (Node, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	v, err := dec.ReadValue()
	if err != nil {
		return Node{}, ErrInvalid
	}
	root := Node{raw: v.Clone()} // before the next read, which reuses v's buffer
	if _, err := dec.ReadToken(); err != io.EOF {
		return Node{}, ErrInvalid // a second value, or trailing garbage
	}
	return root, nil
}

// Kind is '{', '[', '"', '0' (a number), 't', 'f' or 'n'.
func (n Node) Kind() byte { return byte(n.raw.Kind()) }

// Raw is the value's exact text.
func (n Node) Raw() string { return string(n.raw) }

// IsNull reports JSON null.
func (n Node) IsNull() bool { return n.Kind() == 'n' }

// Members returns an object's members; ErrInvalid for any other kind. Parse has already refused duplicates.
func (n Node) Members() (map[string]Node, error) {
	if n.Kind() != '{' {
		return nil, ErrInvalid
	}
	dec := jsontext.NewDecoder(bytes.NewReader(n.raw))
	if _, err := dec.ReadToken(); err != nil {
		return nil, ErrInvalid
	}
	out := map[string]Node{}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, ErrInvalid
		}
		name := tok.String() // a token and a value are valid only until the next read
		v, err := dec.ReadValue()
		if err != nil {
			return nil, ErrInvalid
		}
		out[name] = Node{raw: v.Clone()}
	}
	return out, nil
}

// Elements returns an array's elements; ErrInvalid for any other kind.
func (n Node) Elements() ([]Node, error) {
	if n.Kind() != '[' {
		return nil, ErrInvalid
	}
	dec := jsontext.NewDecoder(bytes.NewReader(n.raw))
	if _, err := dec.ReadToken(); err != nil {
		return nil, ErrInvalid
	}
	var out []Node
	for dec.PeekKind() != ']' {
		v, err := dec.ReadValue()
		if err != nil {
			return nil, ErrInvalid
		}
		out = append(out, Node{raw: v.Clone()})
	}
	return out, nil
}

// Object is an object's members, read with the field rules: absent and null are the same, and a present field of
// the wrong kind is ErrInvalid.
type Object map[string]Node

// AsObject returns the members of an object node.
func AsObject(n Node) (Object, error) {
	m, err := n.Members()
	return Object(m), err
}

// Field returns the member name unless it is absent or null.
func (o Object) Field(name string) (Node, bool) {
	n, ok := o[name]
	if !ok || n.IsNull() {
		return Node{}, false
	}
	return n, true
}

// String returns a string member; ok is false when absent or null.
func (o Object) String(name string) (s string, ok bool, err error) {
	n, ok := o.Field(name)
	if !ok {
		return "", false, nil
	}
	if n.Kind() != '"' {
		return "", false, ErrInvalid
	}
	tok, err := jsontext.NewDecoder(bytes.NewReader(n.raw)).ReadToken()
	if err != nil {
		return "", false, ErrInvalid
	}
	return tok.String(), true, nil
}

// Int returns an integer member: a number written as an integer, without fraction or exponent, within int64.
func (o Object) Int(name string) (v int64, ok bool, err error) {
	n, ok := o.Field(name)
	if !ok {
		return 0, false, nil
	}
	if n.Kind() != '0' {
		return 0, false, ErrInvalid
	}
	v, perr := strconv.ParseInt(string(n.raw), 10, 64)
	if perr != nil {
		return 0, false, ErrInvalid
	}
	return v, true, nil
}

// Float returns a number member as a float64; a number out of range is ErrInvalid.
func (o Object) Float(name string) (v float64, ok bool, err error) {
	n, ok := o.Field(name)
	if !ok {
		return 0, false, nil
	}
	if n.Kind() != '0' {
		return 0, false, ErrInvalid
	}
	v, perr := strconv.ParseFloat(string(n.raw), 64)
	if perr != nil {
		return 0, false, ErrInvalid
	}
	return v, true, nil
}

// Object returns an object member.
func (o Object) Object(name string) (Object, bool, error) {
	n, ok := o.Field(name)
	if !ok {
		return nil, false, nil
	}
	m, err := AsObject(n)
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// Array returns an array member's elements.
func (o Object) Array(name string) ([]Node, bool, error) {
	n, ok := o.Field(name)
	if !ok {
		return nil, false, nil
	}
	e, err := n.Elements()
	if err != nil {
		return nil, false, err
	}
	return e, true, nil
}

// StringMap returns an object member whose values are strings (null is ""). Two names that are canonically
// equivalent ("\u00e9" and "e\u0301") are ErrInvalid: Swift's String-keyed map cannot hold both, so both refuse them.
func (o Object) StringMap(name string) (map[string]string, bool, error) {
	m, ok, err := o.Object(name)
	if err != nil || !ok {
		return nil, ok, err
	}
	out := make(map[string]string, len(m))
	seen := make(map[string]bool, len(m))
	for k := range m {
		nfc := norm.NFC.String(k)
		if seen[nfc] {
			return nil, false, ErrInvalid
		}
		seen[nfc] = true
		s, _, err := m.String(k)
		if err != nil {
			return nil, false, err
		}
		out[k] = s
	}
	return out, true, nil
}

// Code returns a provider error code as text: a string as itself, any other value as its exact JSON text, and ""
// for absence or null. Codes are strings or integers in the wild; keeping the received text keeps both
// implementations equal for anything else.
func (o Object) Code(name string) (string, error) {
	n, ok := o.Field(name)
	if !ok {
		return "", nil
	}
	if n.Kind() == '"' {
		s, _, err := o.String(name)
		return s, err
	}
	return n.Raw(), nil
}
