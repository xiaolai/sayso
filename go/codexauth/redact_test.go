package codexauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaolai/sayso/go/secret"
)

// An Error shows no secret under any verb, on the value or through a pointer, nor as JSON, including a Mode and a
// CodexVersion set by a caller and a secret registered after the error was built.
func TestErrorRedactsUnderEveryVerb(t *testing.T) {
	e := fail(StateUnsupported, "m", errors.New("cause opaque-canary-err-5"))
	e.Mode, e.CodexVersion = "opaque-canary-err-5", "opaque-canary-err-5"
	secret.Register("opaque-canary-err-5")
	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%t"} {
		out = append(out, fmt.Sprintf(verb, e), fmt.Sprintf(verb, *e))
	}
	// %p on a value is a misuse go vet reports, which fmt prints without calling any method; on the pointer it is
	// the address.
	out = append(out, fmt.Sprintf("%p", e))
	for _, v := range []any{e, *e} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	for _, s := range out {
		if strings.Contains(s, "canary") {
			t.Errorf("formatting shows the secret: %s", s)
		}
	}
	if got, want := fmt.Sprintf("%q", e), fmt.Sprintf("%q", e.Error()); got != want {
		t.Errorf("%%q is %s, want %s", got, want)
	}
}
