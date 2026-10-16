package codexauth

import (
	"context"
	"testing"
)

// auth_mode, tokens and access_token as an object, an array, a number and, for tokens, a string, an unused
// credential nested in each: every one keeps its codex-error state. That nothing of them is copied is wipe_test.go's.
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
	for _, json := range cases {
		_, err := fileCreds(t, json).Token(context.Background())
		if StateOf(err) != StateCodexError {
			t.Errorf("%s: %v, want codex-error", json, err)
		}
	}
}
