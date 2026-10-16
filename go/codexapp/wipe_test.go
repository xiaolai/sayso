package codexapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// No byte of a getAuthStatus reply outlives the call but the token the status keeps: the reader's buffer and every
// buffer it outgrows, the result handed to the call and the bytes decoded from it are cleared, on success and on
// every refusal, and a reply that comes after its call stopped waiting is cleared, or never copied (scan_test.go).

// checkLive is check for a client that must still be reading after the scan: a reader that stopped wiped its
// buffer, which could hide a copy the scan is there to find.
func (w *wipeScan) checkLive(t *testing.T, c *Client) {
	t.Helper()
	w.check(t)
	if err := c.k.failed(); err != nil {
		t.Fatalf("the client failed before the scan was done: %v", err)
	}
}

// startReplying starts a child that answers initialize, then answers request i after it with answers[i], built in
// the scan's pages and sent as they are (nil sends nothing); after the last it reads until its input ends, its
// output still open. It needs only /bin/sh.
func (w *wipeScan) startReplying(t *testing.T, answers [][]part, onEvent func(*Client, Event)) *Client {
	t.Helper()
	dir := t.TempDir()
	for i, a := range answers {
		var b []byte
		if a != nil {
			b = w.s.Bytes(w.emit(a))
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("r%02d", i)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "codex")
	src := "#!/bin/sh\nread -r line\necho '{\"id\":1,\"result\":{\"userAgent\":\"codex_cli_rs/0.162.1\"}}'\nread -r line\n" +
		"for f in '" + dir + "'/r*; do\n\tread -r line || exit 0\n\tcat \"$f\"\ndone\nwhile read -r line; do :; done\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}),
		Timeout: 5 * time.Second, OnEvent: onEvent})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAnAuthStatusKeepsOnlyItsToken(t *testing.T) {
	// The third reply is a line of exactly maxLine bytes: the head, the token, the padded middle, a needle, the tail.
	head, middle, tail := `{"id":2,"result":{"authToken":"`, `","pad":"`, `","n":"`
	pad := strings.Repeat("x", maxLine-len(head)-40-len(middle)-len(tail)-40-len(`"}}`))
	for _, c := range []struct {
		name   string
		answer []part
		room   int
	}{
		{"members it ignores", doc(text(`{"id":2,"result":{"authMethod":"chatgpt","authToken":"`), token(),
			text(`","requiresOpenaiAuth":true,"other":"`), needle(), text(`","n":["`), escaped(),
			text(`"]},"junk":"`), needle(), text(`"}`)), 1 << 16},
		{"an escaped token", doc(text(`{"id":2,"result":{"authToken":"`), escapedToken(),
			text(`","authMethod":"chatgpt"}}`)), 1 << 16},
		{"a line of 4 MiB", doc(text(head), token(), text(middle+pad+tail), needle(), text(`"}}`)), 5 << 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWipeScan(t, c.room)
			cl := w.startReplying(t, [][]part{c.answer}, nil)
			st, err := cl.GetAuthStatus(t.Context(), true, false)
			if err != nil {
				t.Fatal(err)
			}
			w.checkLive(t, cl)
			if st.Token() != string(w.s.Bytes(w.needles[0])) {
				t.Error("the status does not hold the token sent")
			}
		})
	}
}

func TestARefusedAuthStatusLeavesNothing(t *testing.T) {
	pad := strings.Repeat("x", maxLine)
	for _, c := range []struct {
		name, want string
		answer     []part
		room       int
		fails      bool // the line fails the client, whose reader then wipes its buffer
	}{
		{"a wrong type after the token", "unexpected getAuthStatus result", doc(text(`{"id":2,"result":{"authToken":"`),
			needle(), text(`","requiresOpenaiAuth":"yes"}}`)), 1 << 16, false},
		{"a method that is no string", "unexpected getAuthStatus result", doc(text(`{"id":2,"result":{"authToken":"`),
			escaped(), text(`","authMethod":7}}`)), 1 << 16, false},
		{"a result that is no object", "unexpected getAuthStatus result", doc(text(`{"id":2,"result":["`), needle(),
			text(`"]}`)), 1 << 16, false},
		{"an error naming a registered secret", "rejected getAuthStatus", doc(
			text(`{"id":2,"error":{"code":-32000,"message":"refused `), registered(), text(` there"}}`)), 1 << 16, false},
		{"a malformed line", "malformed JSON", doc(text(`{"id":2,"result":{"authToken":"`), needle(), text(`"}`)),
			1 << 16, true},
		// The second needle is 2 MiB in, where only the reader's wipe clears it: what is drained after the line is
		// refused is read over the buffer's first bytes only.
		{"a line over 4 MiB", "longer than 4 MiB", doc(text(`{"id":2,"result":{"authToken":"`), needle(),
			text(`","pad":"`+pad[:2<<20]), needle(), text(pad[2<<20:]+`"}}`)), 5 << 20, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWipeScan(t, c.room)
			cl := w.startReplying(t, [][]part{c.answer}, nil)
			_, err := cl.GetAuthStatus(t.Context(), true, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
			var rpc *RPCError
			if errors.As(err, &rpc) && rpc.Message != "refused <redacted> there" {
				t.Errorf("the error message is %q", rpc.Message)
			}
			if c.fails {
				w.check(t)
			} else {
				w.checkLive(t, cl)
			}
		})
	}
}

// A reply that comes after its call stopped waiting, whether the call timed out or was cancelled, is never copied
// and its line is cleared. The child sends it only once the next request arrives, after a notification that tells
// the test the abandoned request was read; the next call returning proves the late line was handled.
func TestALateReplyLeavesNothing(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("cancelled=", cancelled), func(t *testing.T) {
			w := newWipeScan(t, 1<<16)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			onEvent := func(*Client, Event) {
				if cancelled {
					cancel()
				}
			}
			cl := w.startReplying(t, [][]part{
				doc(text(`{"method":"read"}`)),
				doc(text(`{"id":2,"result":{"authToken":"`), needle(), text(`"}}`+"\n"+
					`{"id":3,"result":{"authMethod":"chatgpt","authToken":null}}`)),
			}, onEvent)
			if _, err := cl.GetAuthStatus(ctx, true, false); err == nil {
				t.Fatal("the abandoned call returned no error")
			}
			if st, err := cl.GetAuthStatus(t.Context(), false, false); err != nil || st.Method != "chatgpt" {
				t.Fatalf("the next call: %v, %v", st, err)
			}
			w.checkLive(t, cl)
		})
	}
}

// A reply delivered while its call is still sending, by a call that then gives up, is cleared by the call on its
// way out.
func TestAReplyLeftForACallThatGaveUpIsCleared(t *testing.T) {
	w := newWipeScan(t, 1<<12)
	line := w.s.Bytes(w.emit(doc(text(`{"id":1,"result":{"authToken":"`), needle(), text(`"}}`))))
	k, _, _ := bareCore(t) // its writer never starts, so the call waits to be written
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- k.callRaw(ctx, "getAuthStatus", nil, func([]byte) error { return errors.New("the result was read") })
	}()
	recv(t, "the request to be queued", k.outWake)
	if !k.handle(trimJSONSpace(line)) {
		t.Fatal("the reply was refused")
	}
	cancel()
	if err := recv(t, "the call to give up", done); !errors.Is(err, context.Canceled) {
		t.Fatalf("the call returned %v", err)
	}
	w.check(t)
}

// A reply's line is cleared before its call is given the reply: a call that has its reply leaves no line behind.
func TestAReplysLineIsClearedBeforeItsCallReadsIt(t *testing.T) {
	line := []byte(`{"id":1,"result":{"a":1}}`)
	k, r, start := bareCore(t)
	done := make(chan error, 1)
	go func() {
		done <- k.callRaw(t.Context(), "m", nil, func(result []byte) error {
			switch {
			case string(result) != `{"a":1}`:
				return fmt.Errorf("the result is %q", result)
			case slices.ContainsFunc(line, func(b byte) bool { return b != 0 }):
				return errors.New("the line was not cleared")
			}
			return nil
		})
	}()
	recv(t, "the request to be queued", k.outWake)
	k.outWake <- struct{}{} // the writer's wake-up, taken above
	start()
	readLines(t, r, 1)
	if !k.handle(line) {
		t.Fatal("the reply was refused")
	}
	if err := recv(t, "the call", done); err != nil {
		t.Fatal(err)
	}
}
