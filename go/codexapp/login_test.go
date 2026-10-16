package codexapp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaolai/sayso/go/openaihttp"
)

// loginScript writes a codex stand-in that reports its arguments, folder, environment and first line of input, then
// exits with LOGIN_EXIT.
func loginScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "codex")
	src := "#!/bin/sh\n" +
		"printf 'args=%s\\n' \"$*\"\n" +
		"printf 'pwd=%s\\n' \"$(pwd -P)\"\n" +
		"printf 'key=%s\\n' \"${OPENAI_API_KEY-unset}\"\n" +
		"printf 'home=%s\\n' \"$CODEX_HOME\"\n" +
		"read line; printf 'stdin=%s\\n' \"$line\"\n" +
		"exit \"${LOGIN_EXIT:-0}\"\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// Login runs the flow with the caller's streams, in the configured folder, with CODEX_HOME set and the API key
// removed, as Start runs the app server.
func TestLoginRunsTheFlowAsConfigured(t *testing.T) {
	script := loginScript(t)
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "codex-home")
	for _, device := range []bool{false, true} {
		var out bytes.Buffer
		cfg := Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin", "OPENAI_API_KEY=canary-login"}),
			CodexHome: home, Dir: work}
		if err := Login(cfg, device, Streams{Stdin: strings.NewReader("typed\n"), Stdout: &out, Stderr: &out}); err != nil {
			t.Fatalf("device %v: %v", device, err)
		}
		args := "login"
		if device {
			args = "login --device-auth"
		}
		want := "args=" + args + "\npwd=" + work + "\nkey=unset\nhome=" + home + "\nstdin=typed\n"
		if out.String() != want {
			t.Fatalf("device %v: the flow reported\n%s\nwant\n%s", device, out.String(), want)
		}
	}
}

// A folder Start would refuse is refused by Login too, before Codex runs; a flow that fails is codex-error.
func TestLoginRefusesWhatStartRefuses(t *testing.T) {
	script := loginScript(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"work", filepath.Join(t.TempDir(), "missing"), file} {
		var out bytes.Buffer
		err := Login(Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Dir: dir}, false,
			Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
		if err == nil || !strings.Contains(err.Error(), "working directory") {
			t.Fatalf("dir %q: %v", dir, err)
		}
		if openaihttp.Classify(err) != openaihttp.ClassInternal || out.Len() != 0 {
			t.Fatalf("dir %q: class %s, the flow ran: %q", dir, openaihttp.Classify(err), out.String())
		}
	}
	var out bytes.Buffer
	err := Login(Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin", "LOGIN_EXIT=3"})}, false,
		Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
	if openaihttp.Classify(err) != openaihttp.ClassCodexError {
		t.Fatalf("a failed flow: %v, class %s", err, openaihttp.Classify(err))
	}
}
