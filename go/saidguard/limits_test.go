package saidguard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/fixtures"
)

// The bound- fixtures are inputs that once crashed the generator or took seconds under the size cap; the conformance
// suite checks that each returns false (spec/fixtures.md). Here each must also return well inside a second. The bound
// is loose on purpose: an earlier memo-table matcher took 3.2 and 5.7 seconds on such inputs, so it separates the
// algorithms without timing the machine. The time is CPU time (cpuTime), so a loaded machine does not fail it: at
// load 140 bound-english-rescan took 10.2 s of wall time.
func TestBounds(t *testing.T) {
	cases, err := fixtures.Load("said-guard")
	if err != nil {
		t.Fatal(err)
	}
	timed := 0
	for _, c := range cases {
		if !strings.HasPrefix(c.ID, "bound-") {
			continue
		}
		var f struct{ Script, Transcript, Language string }
		if err := json.Unmarshal(c.Raw, &f); err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		if len(f.Script) > MaxBytes || len(f.Transcript) > MaxBytes {
			t.Fatalf("%s: input exceeds the cap and would not exercise the matcher", c.ID)
		}
		start := cpuTime()
		Matches(f.Transcript, f.Script, f.Language)
		elapsed := cpuTime() - start
		if elapsed > raceFactor*time.Second {
			t.Errorf("%s: took %v", c.ID, elapsed)
		}
		t.Logf("%s: %v", c.ID, elapsed)
		timed++
	}
	if timed == 0 {
		t.Fatal("no bound- fixtures to time")
	}
}

// svParseHan must round-trip: repeated or out-of-order units and overflow are refused, never wrapped.
func TestHanStrict(t *testing.T) {
	bad := []string{"一亿一亿", "一亿一亿一亿", "一万亿一万亿", "万万", "亿万", "零一百", "一百零二十", "一十零五", "一万零五千", "一五一十", "两十", "十十", "一千万万"}
	for _, s := range bad {
		if n, ok := svParseHan([]rune(s)); ok {
			t.Errorf("%s parsed as %d", s, n)
		}
	}
	good := map[string]uint64{"十": 10, "一十": 10, "十二": 12, "一百一": 110, "一百一十": 110, "一百零一": 101, "一百〇五": 105, "两百": 200, "两万五": 25000, "两万五千": 25000, "一万零五": 10005, "一万零五百": 10500, "一千零二十": 1020, "一亿五千万": 150000000, "一万亿": 1e12, "三千五": 3500}
	for s, want := range good {
		if n, ok := svParseHan([]rune(s)); !ok || n != want {
			t.Errorf("%s: got %d %v, want %d", s, n, ok, want)
		}
	}
}

// ParseHanNumeral also reads digit strings, except the two-digit approximations and strings with 两.
func TestParseHanNumeralDigitStrings(t *testing.T) {
	good := map[string]uint64{"一〇八": 108, "二〇二四": 2024, "零": 0, "〇": 0, "两": 2, "七": 7, "一百一": 110}
	for s, want := range good {
		if n, ok := ParseHanNumeral(s); !ok || n != want {
			t.Errorf("%s: got %d %v, want %d", s, n, ok, want)
		}
	}
	for _, s := range []string{"三五", "两三", "一两三", "九九九九九九九九九九九九九九九九九九九九九", ""} {
		if n, ok := ParseHanNumeral(s); ok {
			t.Errorf("%s parsed as %d", s, n)
		}
	}
}

// A long run of spaces is lexed in time linear in its length: only a minus looks across the spaces around it.
func TestLongWhitespaceIsLinear(t *testing.T) {
	text := "5" + strings.Repeat(" ", MaxBytes-2) + "x"
	start := cpuTime()
	Matches(text, text, "en")
	if elapsed := cpuTime() - start; elapsed > raceFactor*time.Second {
		t.Errorf("took %v", elapsed)
	}
}

// A signed operand after spaces ("5 - - 2") keeps the minus before it spoken: "minus negative two" reads it, and
// "negative two" alone does not.
func TestASpacedSignedOperandKeepsTheMinus(t *testing.T) {
	for _, script := range []string{"5 - - 2", "5 - -2", "5 - − 2"} {
		if !Matches("five minus negative two", script, "en") {
			t.Errorf("%q: the full reading is refused", script)
		}
		if Matches("five negative two", script, "en") {
			t.Errorf("%q: the minus may be dropped", script)
		}
	}
}

func TestAnotherSentenceDoesNotMakeACodeShouting(t *testing.T) {
	for _, c := range []struct{ said, script string }{
		{"a p i stop", "API! STOP!"},
		{"stop a p i", "STOP! API!"},
		{"a p i stop", "API. STOP"},
	} {
		if !Matches(c.said, c.script, "en") {
			t.Errorf("%q: %q is refused", c.script, c.said)
		}
	}
	if Matches("a p i stop", "API STOP", "en") {
		t.Error(`"API STOP": a shouted word is read letter by letter`)
	}
}

func TestASpokenDomainDotTakesNoMarkOfALabel(t *testing.T) {
	const script = "Visit ‘foo.dot’ now."
	if !Matches("Visit foo dot dot’ now.", script, "en") {
		t.Error("the mark on its own label is refused")
	}
	if Matches("Visit foo dot’ dot now.", script, "en") {
		t.Error("the mark moves to the spoken dot")
	}
}
