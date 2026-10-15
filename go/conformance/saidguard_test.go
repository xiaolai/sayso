package conformance

import (
	"encoding/json"
	"testing"

	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/saidguard"
)

// Go declares no capabilities, so a true expectation that requires one is exempt; a false expectation binds every
// implementation.
func saidGuard(_ *testing.T, c fixtures.Case) (fixtures.Outcome, string) {
	var f struct {
		Script, Transcript, Language string
		Expect                       bool
	}
	if err := json.Unmarshal(c.Raw, &f); err != nil {
		return fixtures.Fail, err.Error()
	}
	got := saidguard.Matches(f.Transcript, f.Script, f.Language)
	switch {
	case got == f.Expect:
		return fixtures.Pass, ""
	case f.Expect && len(c.Requires) > 0:
		return fixtures.Exempt, ""
	}
	return fixtures.Fail, "got " + map[bool]string{true: "match", false: "no match"}[got]
}

func TestSaidGuard(t *testing.T) { run(t, "said-guard", saidGuard) }
