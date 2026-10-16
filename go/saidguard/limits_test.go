package saidguard

import (
	"strings"
	"testing"
	"time"
)

// Inputs that once crashed the generator or took seconds under the size cap. Each must return false, without
// panicking, well inside a second. The bound is loose on purpose: an earlier memo-table matcher took
// 3.2 and 5.7 seconds here, so it separates the algorithms without timing the machine. The time is CPU time
// (cpuTime), so a loaded machine does not fail it: at load 140 english-rescan took 10.2 s of wall time.
func TestBounds(t *testing.T) {
	cases := []struct{ name, script, transcript string }{
		{"malformed-group", "0.1,000", "wrong"},
		{"malformed-sign", "-,", "wrong"},
		{"malformed-percent", "%1,0000%", "wrong"},
		{"english-rescan", strings.Repeat("one ", 16383), "wrong"},
		{"english-phrase-rescan", strings.Repeat("one hundred and ", 4095), "wrong"},
		{"domain-quadratic", strings.Repeat("dot.dot ", 8191), strings.Repeat("dot ", 16382) + "x"},
		{"domain-middle", strings.Repeat("dot.dot ", 5000), strings.Repeat("dot ", 12500) + "x"},
		{"han-repeat", strings.Repeat("一亿", 10000), "wrong"},
		{"hyphen-chain", strings.Repeat("ab", 8000), strings.Repeat("a-b-", 8000) + "x"},
		{"dash-chain", strings.Repeat("1-", 32767) + "1", "wrong"},
		{"domain-hyphen-labels", strings.Repeat("e-mail.", 4000) + "com", strings.Repeat("email dot e mail dot ", 2000) + "x"},
	}
	for _, c := range cases {
		if len(c.script) > MaxBytes || len(c.transcript) > MaxBytes {
			t.Fatalf("%s: input exceeds the cap and would not exercise the matcher", c.name)
		}
		start := cpuTime()
		got := Matches(c.transcript, c.script, "en")
		if c.name == "han-repeat" {
			got = got || Matches(c.transcript, c.script, "zh")
		}
		elapsed := cpuTime() - start
		if got {
			t.Errorf("%s: matched, want false", c.name)
		}
		if elapsed > raceFactor*time.Second {
			t.Errorf("%s: took %v", c.name, elapsed)
		}
		t.Logf("%s: %v", c.name, elapsed)
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
