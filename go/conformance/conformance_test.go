// Package conformance runs the shared fixtures against the Go implementation and writes results-go.json when
// SAYSO_RESULTS names a path (spec/parity.md).
package conformance

import (
	"os"
	"testing"

	"github.com/xiaolai/sayso/go/internal/fixtures"
)

// components and capabilities are what the Go implementation declares (fixtures/capabilities.json).
var (
	components   = []string{"credentials", "mint", "session", "pcm16", "said-guard"}
	capabilities = []string{}
	results      fixtures.Results
)

func TestMain(m *testing.M) {
	code := m.Run()
	if path := os.Getenv("SAYSO_RESULTS"); path != "" {
		if err := results.Write(path, components, capabilities); err != nil {
			os.Stderr.WriteString("conformance: " + err.Error() + "\n")
			code = 1
		}
	}
	os.Exit(code)
}

// runner checks one case; it returns the outcome and, for a failure, why.
type runner func(t *testing.T, c fixtures.Case) (fixtures.Outcome, string)

func run(t *testing.T, component string, r runner) {
	cases, err := fixtures.Load(component)
	if err != nil {
		t.Fatal(err)
	}
	passed := 0
	for _, c := range cases {
		o, why := r(t, c)
		results.Record(c.ID, o)
		switch o {
		case fixtures.Pass:
			passed++
		case fixtures.Fail:
			t.Errorf("%s: %s", c.ID, why)
		}
	}
	t.Logf("%s: %d fixtures, %d passed", component, len(cases), passed)
}
