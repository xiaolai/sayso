package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/saidguard"
)

// Go declares no capabilities, so a true expectation that requires one is exempt; a false expectation binds every
// implementation.
func saidGuard(_ *testing.T, c fixtures.Case) (fixtures.Outcome, string) {
	var f struct {
		caseHead
		Script     string `json:"script"`
		Transcript string `json:"transcript"`
		Language   string `json:"language"`
		Expect     bool   `json:"expect"`
	}
	if err := decode(c, &f); err != nil {
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

// The revision is shared policy (spec/parity.md): Go's constant is the one capabilities.json records, as Swift's is.
func TestSaidGuardVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures.Dir, "capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		SaidGuardVersion *int `json:"saidGuardVersion"`
	}
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.SaidGuardVersion == nil || *policy.SaidGuardVersion != saidguard.Version {
		t.Fatalf("capabilities.json saidGuardVersion %v, saidguard.Version %d", policy.SaidGuardVersion, saidguard.Version)
	}
}
