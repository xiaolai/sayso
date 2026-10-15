package conformance

import (
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

// decode unmarshals a case's raw JSON into v.
func decode(c fixtures.Case, v any) error {
	if err := json.Unmarshal(c.Raw, v); err != nil {
		return fmt.Errorf("fixture does not decode: %w", err)
	}
	return nil
}
