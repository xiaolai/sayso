package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/xiaolai/sayso/go/internal/fixtures"
)

// outcome turns a list of mismatches into a fixture outcome.
func outcome(problems []string) (fixtures.Outcome, string) {
	if len(problems) == 0 {
		return fixtures.Pass, ""
	}
	return fixtures.Fail, strings.Join(problems, "; ")
}

// normal re-encodes v through JSON, so a Go value and a decoded fixture value compare by their JSON meaning.
func normal(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("unencodable %T", v)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// same reports whether got and want have the same JSON meaning.
func same(got, want any) bool { return reflect.DeepEqual(normal(got), normal(want)) }

// leaks returns the strings of notIn that appear in any of texts.
func leaks(notIn []string, texts ...string) []string {
	var out []string
	for _, s := range notIn {
		for _, t := range texts {
			if strings.Contains(t, s) {
				out = append(out, fmt.Sprintf("%q leaked", s))
				break
			}
		}
	}
	return out
}

// exact re-encodes v through JSON keeping every number as written, so an integer beyond 2^53 compares exactly
// rather than as the nearest float64.
func exact(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("unencodable %T", v)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out any
	_ = d.Decode(&out)
	return out
}

// sameExact reports whether got and want have the same JSON meaning, numbers compared as written. want must come from
// decodeExact.
func sameExact(got, want any) bool { return reflect.DeepEqual(exact(got), exact(want)) }

// decodeExact is decode with numbers kept as written (json.Number), for comparison with sameExact.
func decodeExact(c fixtures.Case, v any) error {
	d := json.NewDecoder(bytes.NewReader(c.Raw))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("fixture does not decode: %w", err)
	}
	return nil
}

// decode unmarshals a case's raw JSON into v.
func decode(c fixtures.Case, v any) error {
	if err := json.Unmarshal(c.Raw, v); err != nil {
		return fmt.Errorf("fixture does not decode: %w", err)
	}
	return nil
}
