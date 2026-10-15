package codexapp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOrder(t *testing.T) {
	root := t.TempDir()
	onPath := filepath.Join(root, "path", "codex")
	cand := filepath.Join(root, "cand", "codex")
	explicit := filepath.Join(root, "explicit", "codex")
	for _, p := range []string{onPath, cand, explicit} {
		writeExec(t, p)
	}
	path := filepath.Dir(onPath)
	cands := []string{filepath.Dir(cand)}
	for _, tc := range []struct {
		name, explicit, path string
		cands                []string
		want                 string
	}{
		{"explicit path wins", explicit, path, cands, explicit},
		{"PATH before candidates", "", path, cands, onPath},
		{"candidates last", "", "/nonexistent", cands, cand},
		{"relative PATH entries are skipped", "", "path", cands, cand},
	} {
		got, err := Resolve(tc.explicit, tc.path, tc.cands)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := Resolve(filepath.Join(root, "missing", "codex"), path, cands); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing explicit path fell through: %v", err)
	}
	if _, err := Resolve("", "/nonexistent", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("nothing to find: %v", err)
	}
	notExec := filepath.Join(root, "plain", "codex")
	if err := os.MkdirAll(filepath.Dir(notExec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notExec, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("", filepath.Dir(notExec), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("a file without an execute bit resolved: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir", "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("", filepath.Join(root, "dir"), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("a folder resolved: %v", err)
	}
}

func TestCandidatesNVMNewestFirst(t *testing.T) {
	home := t.TempDir()
	for _, v := range []string{"v9.0.0", "v20.11.0", "v18.2.1", "v20.9.10"} {
		if err := os.MkdirAll(filepath.Join(home, ".nvm", "versions", "node", v, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := Candidates(home)
	want := []string{"/opt/homebrew/bin", "/usr/local/bin",
		filepath.Join(home, ".local/bin"), filepath.Join(home, ".npm-global/bin"), filepath.Join(home, ".volta/bin"),
		filepath.Join(home, ".bun/bin"), filepath.Join(home, "Library/pnpm")}
	for _, v := range []string{"v20.11.0", "v20.9.10", "v18.2.1", "v9.0.0"} {
		want = append(want, filepath.Join(home, ".nvm", "versions", "node", v, "bin"))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates\n%v\nwant\n%v", got, want)
	}
}

func TestChildEnv(t *testing.T) {
	env := []string{"HOME=/h", "PATH=/inherited", "OPENAI_API_KEY=a", "openai_api_key=b", "Codex_Access_Token=c",
		"CODEX_REFRESH_TOKEN_URL_OVERRIDE=d", "CODEX_REVOKE_TOKEN_URL_OVERRIDE=e", "CODEX_API_KEY=f",
		"CODEX_APP_SERVER_LOGIN_CLIENT_ID=g", "CODEX_HOME=/old", "KEEP=1"}
	got := ChildEnv(env, "/x/bin/codex", "/new")
	want := []string{"HOME=/h", "KEEP=1", "PATH=/x/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/inherited", "CODEX_HOME=/new"}
	if !slices.Equal(got, want) {
		t.Fatalf("env %v\nwant %v", got, want)
	}
	if got := ChildEnv([]string{"CODEX_HOME=/keep"}, "/b/codex", ""); !slices.Contains(got, "CODEX_HOME=/keep") {
		t.Fatalf("an unset codexHome dropped the inherited CODEX_HOME: %v", got)
	}
}

func TestVersion(t *testing.T) {
	for ua, want := range map[string]string{
		"codex_cli_rs/0.162.1 (Mac OS 15.0; arm64)": "0.162.1",
		"codex_cli_rs/0.162.0":                      "0.162.0",
		"codex_cli_rs":                              "",
		"codex_cli_rs/beta":                         "",
		"":                                          "",
	} {
		if got := Version(ua); got != want {
			t.Errorf("Version(%q) = %q, want %q", ua, got, want)
		}
	}
	if !VersionLess("0.161.9", "0.162.0") || VersionLess("0.162.0", "0.162.0") || VersionLess("0.163", "0.162.9") {
		t.Fatal("VersionLess")
	}
}

func TestStartRefusals(t *testing.T) {
	if _, err := Start(t.Context(), Config{Bin: "codex", Timeout: 1}); err == nil {
		t.Error("relative path accepted")
	}
	if _, err := Start(t.Context(), Config{Bin: "/bin/false"}); err == nil {
		t.Error("missing timeout accepted")
	}
	_, err := Start(t.Context(), Config{Bin: "/bin/false", Timeout: 1, Guard: func(string) error { return errors.New("not in tests") }})
	if err == nil || !strings.Contains(err.Error(), "not in tests") {
		t.Errorf("guard ignored: %v", err)
	}
}

// A file whose only execute bit is for another class cannot start, so the search goes on past it.
func TestResolveNeedsExecutePermissionForThisUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may execute any file with an execute bit")
	}
	root := t.TempDir()
	first, second := filepath.Join(root, "a", "codex"), filepath.Join(root, "b", "codex")
	writeExec(t, first)
	writeExec(t, second)
	if err := os.Chmod(first, 0o641); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve("", filepath.Dir(first)+":"+filepath.Dir(second), nil)
	if err != nil || got != second {
		t.Fatalf("resolved %q, %v; want %q", got, err, second)
	}
	if _, err := Resolve(second+"\x00suffix", "", nil); err == nil {
		t.Fatal("a path with a NUL resolved")
	}
}

// Formatting a Config never prints the environment it carries.
func TestConfigRedactsEnvironment(t *testing.T) {
	cfg := Config{Bin: "/b/codex", Env: []string{"OPENAI_API_KEY=canary-env-1"}}
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg), fmt.Sprint(&cfg)} {
		if strings.Contains(s, "canary") {
			t.Fatalf("leaked: %s", s)
		}
	}
}

// A child that stops reading cannot hold a call past its timeout, nor Close past the grace.
func TestCallTimesOutWhenTheChildStopsReading(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	// Answers initialize, then never reads again.
	body := "#!" + python + "\nimport sys, json, time\nm = json.loads(sys.stdin.readline())\n" +
		"print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\ntime.sleep(30)\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// Generous enough for a loaded machine to start Python and answer initialize.
	c, err := Start(t.Context(), Config{Bin: script, Env: []string{"PATH=/usr/bin:/bin"}, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = c.Call(t.Context(), "big", map[string]any{"pad": strings.Repeat("x", 4<<20)}, nil) // far beyond a pipe buffer
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("call returned %v after %v", err, time.Since(start))
	}
	start = time.Now()
	_ = c.Close()
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("close took %v", d)
	}
}
