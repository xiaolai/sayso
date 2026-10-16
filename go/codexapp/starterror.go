package codexapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"syscall"

	"github.com/xiaolai/sayso/go/secret"
)

// StartError is a Start that failed once the child was running: the failure, how the child ended when it exited by
// itself before Start stopped it, and what it wrote to stderr. The failure's class is found through Unwrap. The
// stderr is never printed, by Error, fmt, slog or encoding/json: the scrubber cannot recognise every credential
// format, so showing it is the caller's choice, as with Client.Stderr.
type StartError struct {
	Err error
	// ExitStatus is "exit status N" or "signal N" for a child that exited by itself, "" otherwise.
	ExitStatus string
	stderr     func() string // a closure, so reflection cannot read it either
}

func (e *StartError) Error() string {
	s := "codexapp: start failed"
	if e.Err != nil {
		s = e.Err.Error()
	}
	if e.ExitStatus != "" {
		s += " (" + e.ExitStatus + ")"
	}
	return secret.Scrub(s)
}

func (e *StartError) Unwrap() error { return e.Err }

// Stderr returns the first 64 KiB the child wrote to stderr, scrubbed when captured and again when read.
func (e *StartError) Stderr() string {
	if e.stderr == nil {
		return ""
	}
	return secret.Scrub(e.stderr())
}

// Format prints Error for every verb, on the value and through a pointer.
func (e StartError) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = f.Write([]byte(strconv.Quote(e.Error())))
		return
	}
	_, _ = f.Write([]byte(e.Error()))
}

// MarshalJSON encodes Error and the exit status.
func (e StartError) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"error": e.Error(), "exitStatus": e.ExitStatus})
}

// LogValue logs Error.
func (e StartError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// exitStatus is how the child ended, as both implementations print it: "exit status N" or "signal N".
func exitStatus(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	switch {
	case !ok:
		return ""
	case ws.Exited():
		return "exit status " + strconv.Itoa(ws.ExitStatus())
	case ws.Signaled():
		return "signal " + strconv.Itoa(int(ws.Signal()))
	}
	return ""
}
