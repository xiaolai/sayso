package codexapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/xiaolai/sayso/go/failure"
)

// A failed start's stderr reaches the caller through Stderr only, unscrubbed formats included: no formatting path,
// JSON, slog or reflection shows it, while the failure's class and the exit status stay readable.
func TestStartErrorNeverPrintsStderr(t *testing.T) {
	const canary = "start-stderr-canary-unregistered"
	e := &StartError{Err: codexError("Codex app-server exited unexpectedly", nil), ExitStatus: "exit status 3",
		stderr: func() string { return canary }}
	opaque(t, "StartError", e, canary)
	opaque(t, "StartError value", *e, canary)
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("start", "err", e, "value", *e)
	slog.New(slog.NewTextHandler(&logged, nil)).Info("start", "err", e, "value", *e)
	for _, v := range []any{e, *e} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		logged.Write(b)
	}
	if strings.Contains(logged.String(), canary) {
		t.Error("JSON or slog shows the stderr")
	}
	if e.Stderr() != canary {
		t.Errorf("Stderr is %q", e.Stderr())
	}
	if !strings.HasSuffix(e.Error(), "unexpectedly (exit status 3)") {
		t.Errorf("Error is %q", e.Error())
	}
	var f *failure.Error
	if !errors.As(e, &f) || f.Class != failure.ClassCodexError {
		t.Error("the failure's class is not found through Unwrap")
	}
	if got := (&StartError{}).Error(); got != "codexapp: start failed" || (&StartError{}).Stderr() != "" {
		t.Errorf("the zero StartError: %q", got)
	}
}

// The exit status is written as Swift writes it, from a real wait status.
func TestExitStatusWording(t *testing.T) {
	for script, want := range map[string]string{"exit 3": "exit status 3", "exit 0": "exit status 0", "kill -9 $$": "signal 9"} {
		cmd := exec.Command("/bin/sh", "-c", script)
		_ = cmd.Run()
		if got := exitStatus(cmd.ProcessState); got != want {
			t.Errorf("%s: %q, want %q", script, got, want)
		}
	}
	if exitStatus(nil) != "" {
		t.Error("no process state names an exit")
	}
}
