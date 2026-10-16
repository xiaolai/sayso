package codexauth

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// auth_mode, tokens and access_token as an object, an array, a number and, for tokens, a string, an unused
// credential nested in each: every one keeps its codex-error state, and the nested value is refused from its bytes,
// never decoded as a string.
func TestNonStringMembersAreNotDecoded(t *testing.T) {
	const canary = "nested-unused-credential-c4e1"
	const access = "members-test-token-8f2b"
	var cases []string
	for _, v := range []string{`{"refresh_token":"` + canary + `"}`, `["` + canary + `"]`, `7`} {
		cases = append(cases,
			`{"auth_mode":`+v+`,"tokens":{"access_token":"`+access+`"}}`,
			`{"auth_mode":"chatgpt","tokens":{"access_token":`+v+`}}`,
			`{"auth_mode":"chatgpt","tokens":`+v+`}`,
			`{"tokens":{"access_token": `+v+`}}`) // the value after a space
	}
	cases = append(cases, `{"tokens":"`+canary+`\n"}`) // a string tokens, escaped: unquoting would copy it
	seen := false
	stringDecoded = func(raw []byte) { seen = seen || bytes.Contains(raw, []byte(canary)) }
	defer func() { stringDecoded = nil }()
	for _, json := range cases {
		seen = false
		_, err := fileCreds(t, json).Token(context.Background())
		if StateOf(err) != StateCodexError {
			t.Errorf("%s: %v, want codex-error", json, err)
		}
		if seen {
			t.Errorf("%s: the nested credential was decoded", json)
		}
	}
}

// The hook sees a string decoded: a string member is, and an unknown string mode keeps its state.
func TestTheHookSeesADecodedString(t *testing.T) {
	const canary = "watched-mode-5d3a"
	seen := false
	stringDecoded = func(raw []byte) { seen = seen || bytes.Contains(raw, []byte(canary)) }
	defer func() { stringDecoded = nil }()
	_, err := fileCreds(t, `{"auth_mode":"`+canary+`"}`).Token(context.Background())
	if StateOf(err) != StateCodexError || !strings.Contains(err.Error(), "unknown auth_mode") {
		t.Errorf("an unknown string mode: %v", err)
	}
	if !seen {
		t.Error("a string member did not reach the decoder")
	}
}
