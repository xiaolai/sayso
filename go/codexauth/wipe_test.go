package codexauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"testing"

	"github.com/xiaolai/sayso/go/internal/memscan"
	"github.com/xiaolai/sayso/go/internal/prereq"
)

// No byte of auth.json outlives a read except the access token kept: every buffer the reader reads it into,
// validates it in, or decodes a member of, is a view of the one cleared buffer or is cleared before it is dropped. A
// scan of the process's writable memory, dropped garbage included, finds no copy of a value the reader did not keep,
// on the success path and on every refusal.

type part struct {
	text string
	kind byte // 0 text, 'n' a needle, 'e' a needle written as \u escapes, 'd' a needle of digits, 'j' a JWT
	jwt  []part
}

func text(s string) part   { return part{text: s} }
func needle() part         { return part{kind: 'n'} }
func escaped() part        { return part{kind: 'e'} }
func digits() part         { return part{kind: 'd'} }
func jwtOf(p ...part) part { return part{kind: 'j', jwt: p} }
func doc(p ...part) []part { return p }

const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// emit appends parts to the scan's pages; a JWT's payload is replaced there by its encoding.
func emit(s *memscan.Scan, parts []part) {
	for _, p := range parts {
		switch p.kind {
		case 0:
			s.Append(p.text)
		case 'n':
			s.AppendBytes(s.Bytes(s.Needle(40)))
		case 'e':
			for _, b := range s.Bytes(s.Needle(40)) {
				s.Append(fmt.Sprintf(`\u%04x`, b))
			}
		case 'd':
			r := s.Needle(40)
			s.Digits(r)
			s.AppendBytes(s.Bytes(r))
		case 'j':
			s.Append("eyJhbGciOiJub25lIn0.")
			start := s.Mark()
			emit(s, p.jwt)
			plain := s.Bytes([2]int{start, s.Mark()})
			var enc []byte // the encoding's bytes hold no needle
			for i := 0; i < len(plain); i += 3 {
				n := min(3, len(plain)-i)
				var v uint32
				for k := 0; k < 3; k++ {
					v <<= 8
					if k < n {
						v |= uint32(plain[i+k])
					}
				}
				for k := 0; k <= n; k++ {
					enc = append(enc, b64url[v>>(18-6*k)&63])
				}
			}
			s.Reset(start)
			s.AppendBytes(enc)
			s.Append(".c2ln")
		}
	}
}

// readCase writes the document parts make to a fresh auth.json, reads it, and returns the state with how often each
// needle is found in memory afterwards.
func readCase(t *testing.T, parts []part) (State, []int) {
	t.Helper()
	s, err := memscan.New(1 << 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// No collection runs until the scan is done: a collection can return a span of garbage to the system, which
	// zeroes it, and a copy dropped unwiped would then go unseen.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	start := s.Mark()
	emit(s, parts)
	base := tree(t)
	codexHome := filepath.Join(base, "confine", "a", "b")
	f, err := os.OpenFile(filepath.Join(codexHome, "auth.json"), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(s.Bytes([2]int{start, s.Mark()})); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	loc, e := authFilePath(codexHome, base, filepath.Join(base, "confine"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = readAuthFile(loc, nil)
	loc.close()
	found, err := s.Occurrences()
	if errors.Is(err, memscan.ErrUnsupported) {
		prereq.Missing(t, err.Error())
	}
	if e == nil {
		return StateReady, found
	}
	return e.State, found
}

func assertNoCopy(t *testing.T, parts []part, want State) {
	t.Helper()
	got, found := readCase(t, parts)
	if got != want {
		t.Errorf("state %s, want %s", got, want)
	}
	if slices.ContainsFunc(found, func(n int) bool { return n != 0 }) {
		t.Errorf("a value outlived the read: needles found %v", found)
	}
}

// The scan finds a copy dropped unwiped: what the other tests rely on.
func TestTheScanFindsADroppedCopy(t *testing.T) {
	s, err := memscan.New(1 << 12)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := s.Needle(40)
	sink = append([]byte(nil), s.Bytes(r)...)
	sink = nil
	found, err := s.Occurrences()
	if errors.Is(err, memscan.ErrUnsupported) {
		prereq.Missing(t, err.Error())
	}
	if found[0] == 0 {
		t.Error("the scan missed a dropped copy")
	}
}

var sink []byte

func TestASignInKeepsOnlyTheAccessToken(t *testing.T) {
	assertNoCopy(t, doc(
		text(`{"auth_mode":"chatgpt","tokens":{"access_token":"`),
		jwtOf(text(`{"exp":1800003600,"c":"`), needle(), text(`","e":"`), escaped(), text(`"}`)),
		text(`","refresh_token":"`), needle(), text(`","id_token":{"n":["`), escaped(),
		text(`"]}},"OPENAI_API_KEY":null,"`), needle(), text(`":1,"`), escaped(),
		text(`":2,"last_refresh":`), digits(), text("}"),
	), StateReady)
}

func TestARefusedSignInLeavesNothing(t *testing.T) {
	for _, c := range []struct {
		parts []part
		want  State
	}{
		{doc(text(`{"OPENAI_API_KEY":"`), needle(), text(`"}`)), StateAPIKey},
		{doc(text(`{"personal_access_token":"`), escaped(), text(`"}`)), StateUnsupported},
		{doc(text(`{"auth_mode":"`), needle(), text(`"}`)), StateCodexError},
		{doc(text(`{"auth_mode":"`), escaped(), text(`"}`)), StateCodexError},
		{doc(text(`{"auth_mode":{"k":"`), needle(), text(`"}}`)), StateCodexError},
		{doc(text(`{"auth_mode":["`), escaped(), text(`"]}`)), StateCodexError},
		{doc(text(`{"tokens":["`), needle(), text(`"]}`)), StateCodexError},
		{doc(text(`{"tokens":"`), escaped(), text(`"}`)), StateCodexError},
		{doc(text(`{"tokens":{"access_token":{"k":"`), needle(), text(`"}}}`)), StateCodexError},
		{doc(text(`{"tokens":{"access_token":7,"refresh_token":"`), needle(), text(`"}}`)), StateCodexError},
	} {
		assertNoCopy(t, c.parts, c.want)
	}
}

func TestAnUnreadableFileLeavesNothing(t *testing.T) {
	// A duplicate name, cut short, a control character, a number out of range, not an object: refused by
	// validation, each holding a needle the validator read.
	for _, parts := range [][]part{
		doc(text(`{"`), escaped(), text(`":1,"x":"`), needle(), text(`","x":2}`)),
		doc(text(`{"tokens":{"refresh_token":"`), needle()),
		doc(text(`{"k":"`), needle(), text("\x01\"}")),
		doc(text(`{"n":`), digits(), text(`e999,"k":"`), needle(), text(`"}`)),
		doc(text(`["`), needle(), text(`"]`)),
	} {
		assertNoCopy(t, parts, StateCodexError)
	}
}
