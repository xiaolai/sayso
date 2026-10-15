package secret

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSecretsNeverFormat(t *testing.T) {
	tok := NewToken("eyJcanary.canary.canary", time.Unix(0, 0), "plus", "codex")
	cs := NewClientSecret("ek_canary", time.Unix(0, 0))
	for _, v := range []any{tok, &tok, cs, &cs, []any{tok, cs}, struct{ T Token }{tok}} {
		out := fmt.Sprintf("%v %+v %#v %s %q %x %d", v, v, v, v, v, v, v)
		js, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		for _, s := range []string{out, string(js)} {
			if strings.Contains(s, "canary") {
				t.Errorf("%T leaked: %s", v, s)
			}
			if !strings.Contains(s, "redacted") { // json.Marshal escapes < and >
				t.Errorf("%T: no redaction marker in %s", v, s)
			}
		}
	}
	if tok.Bearer() != "eyJcanary.canary.canary" || cs.Bearer() != "ek_canary" {
		t.Fatal("Bearer() must return the secret")
	}
}

func TestConstructorsRegisterOpaqueSecrets(t *testing.T) {
	_ = NewToken("opaque-canary-token-77", time.Time{}, "", "")
	_ = NewClientSecret("opaque-canary-secret-88", time.Time{})
	if got := Scrub("a opaque-canary-token-77 b opaque-canary-secret-88"); got != "a <redacted> b <redacted>" {
		t.Fatalf("Scrub = %q", got)
	}
}

func TestLongestSecretFirst(t *testing.T) {
	Register("abc-secret")
	Register("abc-secret-longer")
	if got := Scrub("x abc-secret-longer y"); got != "x <redacted> y" {
		t.Fatalf("Scrub = %q", got)
	}
}

func TestPatterns(t *testing.T) {
	if got := Scrub("Bearer eyJa.eyJb.c and ek_live_1"); got != "Bearer <redacted> and <redacted>" {
		t.Fatalf("Scrub = %q", got)
	}
}
