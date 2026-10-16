package canon

import (
	"slices"
	"strings"
	"testing"
)

// accept: VER-03#1
func TestEquivalencePairs(t *testing.T) {
	pairs := [][2]string{
		{"Mrs. Okafor", "Missus Okafor"},
		{"42", "forty-two"},
		{"1999", "nineteen ninety-nine"},
		{"3.5%", "three point five percent"},
		{"四十二", "42"},
		{"two thousand", "2,000"},
		// Further cases from design §8.3.
		{"the 16th", "the sixteenth"},
		{"1999", "one thousand nine hundred ninety-nine"},
		{"1905", "nineteen oh five"},
		{"一千二百八十", "1280"},
		{"两万", "20,000"},
		{"-7", "minus seven"},
		{"one hundred and five", "105"},
		{"e-mail", "email"},
		{"e.g. this", "for example this"},
		{"Dr. Who vs. Mr. Smith", "doctor who versus mister smith"},
		{"St. Louis", "Saint Louis"},
		{"Main St.", "main street"},
		{"Don't", "dont"},
		{"ＨＥＬＬＯ，　ｗｏｒｌｄ！", "hello world"},
		// A code spelled out loud is transcribed with or without spaces between its letters.
		{"part XJ 4471 B", "part X J 4471 B"},
		{"account DE89", "account D E eighty-nine"},
		{"ticket JIRA-10442", "ticket J I R A 10442"},
		{"use the A P I", "use the API"},
		// Zero is read "oh" in digit readings, and STT writes it "O", "oh" or "0".
		{"three seven oh four, oh oh four four", "three seven O four, O O four four"},
		{"DE eighty-nine three seven oh four", "D E eighty nine three seven O four"},
		{"version 4.0", "version four point oh"},
		// "a hundred" is one hundred, and STT writes it as digits: a calm take of "about a hundred and
		// sixty" was refused on every attempt for being heard as "about 160".
		{"about a hundred and sixty is normal", "about 160 is normal"},
		{"a thousand times", "1,000 times"},
		{"a million and one", "one million and one"},
	}
	for _, p := range pairs {
		if !Equivalent(p[0], p[1]) {
			t.Errorf("Equivalent(%q, %q) = false; canon %q vs %q", p[0], p[1], Canon(p[0]), Canon(p[1]))
		}
	}
	for _, p := range [][2]string{{"42", "forty-three"}, {"1999", "nineteen ninety-eight"}, {"3.5", "35"}, {"四十二", "24"},
		// Joining spelled letters removes spacing only: different or extra letters still differ.
		{"part X J", "part X K"}, {"plan A", "plan A B"}, {"XJ", "JX"},
		// "oh" is zero only beside a number word.
		{"oh no", "zero no"}, {"the letter O", "the letter zero"},
		// "a" is one only before a scale word: not an article elsewhere, and not two hundred.
		{"a hundred", "200"}, {"a cat", "one cat"}, {"a hundred", "a"}} {
		if Equivalent(p[0], p[1]) {
			t.Errorf("Equivalent(%q, %q) = true", p[0], p[1])
		}
	}
}

func TestCanonForms(t *testing.T) {
	cases := map[string]string{
		"1,234,567":        "one million two hundred thirty four thousand five hundred sixty seven",
		"007":              "zero zero seven",
		"3.14":             "three point one four",
		"1st 2nd 3rd 21st": "first second third twenty first",
		"20th":             "twentieth",
		"1,2":              "one two",
		"mp3":              "mp three",
		"etc.":             "et cetera",
		"i.e. it":          "that is it",
		"一九九九":             "one thousand nine hundred ninety nine",
		"一亿两千万":            "one hundred twenty million",
		"Hello—world":      "hello world",
		"msð":              "msð", // "ms" is not a whole word here, though regexp's \b says so
		"X J 4471":         "xj four thousand four hundred seventy one",
		"Oh no, O O seven": "oh no zero zero seven",
	}
	for in, want := range cases {
		if got := strings.Join(Canon(in), " "); got != want {
			t.Errorf("Canon(%q) = %q, want %q", in, got, want)
		}
	}
}

// accept: VER-03#2
func FuzzCanon(f *testing.F) {
	for _, s := range []string{"Mrs. Okafor", "3.5%", "四十二", "2,000", "-1st", "e.g."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c := Canon(s)
		if again := Canon(strings.Join(c, " ")); !slices.Equal(again, c) {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, c, again)
		}
		if !Equivalent(s, s) {
			t.Fatalf("not reflexive: %q", s)
		}
	})
}
