package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/codexapp"
	"github.com/xiaolai/sayso/go/codexauth"
	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

// fakeServer is the scripted codex both suites run (spec/fixtures.md).
var fakeServer = filepath.Join("..", "..", "tools", "fake-app-server.py")

type credSetup struct {
	Now           int64             `json:"now"`
	MarginSeconds int64             `json:"marginSeconds"`
	Source        string            `json:"source"`
	Codex         string            `json:"codex"`
	InheritedPath *string           `json:"inheritedPath"`
	InheritedEnv  map[string]string `json:"inheritedEnv"`
	CodexHome     string            `json:"codexHome"`
	File          *credFile         `json:"file"`
	Retry         *credFile         `json:"retry"`
	TimeoutMs     int64             `json:"timeoutMs"`
	CancelAfterMs int64             `json:"cancelAfterMs"`
	Server        json.RawMessage   `json:"server"`
	Client        *clientSetup      `json:"client"`
}

type credFile struct {
	Kind      string          `json:"kind"`
	JSON      json.RawMessage `json:"json"`
	Raw       *string         `json:"raw"`
	Hex       *string         `json:"hex"`
	RawRepeat *struct {
		Text  string `json:"text"`
		Count int    `json:"count"`
	} `json:"rawRepeat"`
	PadTo int `json:"padTo"`
}

type credStep struct {
	Note   string         `json:"note"`
	At     int64          `json:"at"`
	Op     string         `json:"op"`
	Stale  string         `json:"stale"`
	Expect map[string]any `json:"expect"`
}

type credCase struct {
	caseHead
	Setup  credSetup      `json:"setup"`
	Expect map[string]any `json:"expect"`
	Steps  []credStep     `json:"steps"`
}

// world is one case's temporary machine: a home folder, the codex installation and the fake's record.
type world struct {
	root, home, binDir, codexHome, record string
	cfg                                   codexauth.Config
	logs                                  *bytes.Buffer
	now                                   time.Time
	cancelAfter                           time.Duration // the caller cancels the probe this long after it starts
}

func TestCredentials(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("the credentials fixtures need python3 for the fake app-server")
	}
	fake, err := filepath.Abs(fakeServer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fake); err != nil {
		t.Fatalf("fake app-server: %v", err)
	}
	run(t, "credentials", func(t *testing.T, c fixtures.Case) (fixtures.Outcome, string) {
		var cc credCase
		if err := decode(c, &cc); err != nil {
			return fixtures.Fail, err.Error()
		}
		w, err := build(t, cc.Setup, python, fake)
		if err != nil {
			return fixtures.Fail, "setup: " + err.Error()
		}
		if cc.Setup.Client != nil {
			return outcome(w.client(t, cc.Setup.Client, cc.Expect))
		}
		if cc.Steps != nil {
			return outcome(w.timeline(cc))
		}
		return outcome(w.probe(cc.Expect))
	})
}

func build(t *testing.T, s credSetup, python, fake string) (*world, error) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		return nil, err
	}
	w := &world{root: root, home: filepath.Join(root, "home"), binDir: filepath.Join(root, "bin"),
		record: filepath.Join(root, "record.jsonl"), logs: &bytes.Buffer{}}
	w.now = time.Unix(cmp(s.Now, 1800000000), 0)
	w.cancelAfter = time.Duration(s.CancelAfterMs) * time.Millisecond
	t.Cleanup(func() { // what the fake started outlives a failing case; end it with the case
		rec, err := w.read()
		if err != nil {
			return
		}
		// Only a process still in the group it was started in: a pid freed and reused is not ours to kill.
		for _, pid := range rec.group {
			if g, err := syscall.Getpgid(pid); err == nil && slices.Contains(rec.pids, g) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		for _, pid := range rec.other {
			if g, err := syscall.Getpgid(pid); err == nil && g == pid {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	for _, d := range []string{w.home, w.binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	scenario := filepath.Join(root, "scenario.json")
	server := s.Server
	if len(server) == 0 || string(server) == "null" {
		server = json.RawMessage("{}")
	}
	if err := os.WriteFile(scenario, server, 0o600); err != nil {
		return nil, err
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexec '%s' -I '%s' \"$@\"\n", python, fake)
	explicit := ""
	install := func(dir string) error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "codex"), []byte(wrapper), 0o755)
	}
	switch kind, arg, _ := strings.Cut(s.Codex, ":"); kind {
	case "fake":
		if err := install(w.binDir); err != nil {
			return nil, err
		}
	case "npm-shim":
		shim := fmt.Sprintf("#!/bin/sh\nshift\nexec '%s' -I '%s' \"$@\"\n", python, fake) // drops the script path env passes
		if err := os.WriteFile(filepath.Join(w.binDir, "sayso-fake-node"), []byte(shim), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(w.binDir, "codex"), []byte("#!/usr/bin/env sayso-fake-node\n"), 0o755); err != nil {
			return nil, err
		}
		explicit = filepath.Join(w.binDir, "codex")
	case "", "none":
	case "explicit-missing":
		explicit = filepath.Join(w.home, "missing", "codex")
	case "candidate":
		w.binDir = filepath.Join(w.home, arg)
		if err := install(w.binDir); err != nil {
			return nil, err
		}
	case "nvm":
		for _, v := range strings.Split(arg, ",") {
			if err := install(filepath.Join(w.home, ".nvm", "versions", "node", v, "bin")); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unknown codex setup %q", s.Codex)
	}

	codexHome := ""
	w.codexHome = filepath.Join(w.home, ".codex")
	switch s.CodexHome {
	case "", "default":
	case "inside":
		codexHome = filepath.Join(w.home, "custom-codex")
		w.codexHome = codexHome
	case "outside":
		codexHome = filepath.Join(root, "outside")
		w.codexHome = codexHome
	case "symlink-outside":
		if err := os.MkdirAll(filepath.Join(root, "outside"), 0o700); err != nil {
			return nil, err
		}
		codexHome = filepath.Join(w.home, "link")
		if err := os.Symlink(filepath.Join(root, "outside"), codexHome); err != nil {
			return nil, err
		}
		w.codexHome = filepath.Join(root, "outside")
	case "file-not-folder":
		codexHome = filepath.Join(w.home, "codex-file")
		if err := os.WriteFile(codexHome, nil, 0o600); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown codexHome %q", s.CodexHome)
	}
	if s.CodexHome != "file-not-folder" {
		if err := os.MkdirAll(w.codexHome, 0o700); err != nil {
			return nil, err
		}
		if err := placeFile(w, s.File); err != nil {
			return nil, err
		}
	}

	path := w.binDir + ":/usr/bin:/bin"
	if s.InheritedPath != nil {
		path = *s.InheritedPath
	}
	env := []string{"HOME=" + w.home, "PATH=" + path, "SAYSO_FAKE_SCENARIO=" + scenario, "SAYSO_FAKE_RECORD=" + w.record}
	for k, v := range s.InheritedEnv {
		env = append(env, k+"="+v)
	}
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	var candidates []string
	for _, d := range codexapp.Candidates(w.home) {
		if strings.HasPrefix(d, w.home+"/") { // never the machine's own /opt/homebrew/bin or /usr/local/bin
			candidates = append(candidates, d)
		}
	}
	source := codexauth.SourceAuto
	if s.Source == "file" {
		source = codexauth.SourceFile
	}
	w.cfg = codexauth.Config{
		Source: source, Bin: explicit, Home: w.home, ConfineCodexHome: w.home, Env: codexapp.EnvOf(env), Candidates: candidates,
		Margin: time.Duration(cmp(s.MarginSeconds, 600)) * time.Second, Timeout: time.Duration(cmp(s.TimeoutMs, 5000)) * time.Millisecond,
		Now:    func() time.Time { return w.now },
		Logger: slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Guard: func(p string) error {
			if !strings.HasPrefix(p, root+"/") {
				return fmt.Errorf("%s is outside the case's temporary folder", p)
			}
			return nil
		},
	}
	if s.Retry != nil {
		retry := *s.Retry
		w.cfg.OnFileRetry = func() {
			if err := writeFileContent(filepath.Join(w.codexHome, "auth.json"), &retry); err != nil {
				t.Errorf("retry content: %v", err)
			}
		}
	}
	return w, nil
}

func cmp(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func placeFile(w *world, f *credFile) error {
	if f == nil {
		return nil
	}
	target := filepath.Join(w.codexHome, "auth.json")
	switch f.Kind {
	case "missing":
		return nil
	case "regular":
		return writeFileContent(target, f)
	case "symlink":
		elsewhere := filepath.Join(w.home, "elsewhere", "auth.json")
		if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
			return err
		}
		if err := writeFileContent(elsewhere, f); err != nil {
			return err
		}
		return os.Symlink(elsewhere, target)
	case "fifo":
		return syscall.Mkfifo(target, 0o600)
	case "directory":
		return os.Mkdir(target, 0o700)
	}
	return fmt.Errorf("unknown file kind %q", f.Kind)
}

func writeFileContent(path string, f *credFile) error {
	var data []byte
	switch {
	case f.Raw != nil:
		data = []byte(*f.Raw)
	case f.Hex != nil:
		b, err := hex.DecodeString(*f.Hex)
		if err != nil {
			return err
		}
		data = b
	case f.RawRepeat != nil:
		data = []byte(strings.Repeat(f.RawRepeat.Text, f.RawRepeat.Count))
	case f.JSON != nil:
		var v any
		if err := json.Unmarshal(f.JSON, &v); err != nil {
			return err
		}
		b, err := json.Marshal(fixtures.Substitute(v))
		if err != nil {
			return err
		}
		data = b
		if pad := f.PadTo - len(data); pad > 0 {
			data = append(data, strings.Repeat(" ", pad)...)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recorded is what the fake app-server wrote.
type recorded struct {
	spawns   []spawn
	requests []map[string]any // every message received, in order
	pids     []int
	group    []int // processes the fake started in its own process group
	other    []int // processes the fake started in a session of their own
}

type spawn struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

func (w *world) read() (recorded, error) {
	var r recorded
	data, err := os.ReadFile(w.record)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var line struct {
			Spawn           *spawn         `json:"spawn"`
			Req             map[string]any `json:"req"`
			PID             int            `json:"pid"`
			GroupDescendant int            `json:"groupDescendant"`
			Descendant      int            `json:"descendant"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return r, err
		}
		switch {
		case line.Spawn != nil:
			r.spawns = append(r.spawns, *line.Spawn)
			r.pids = append(r.pids, line.PID)
		case line.Req != nil:
			r.requests = append(r.requests, line.Req)
		case line.GroupDescendant > 0:
			r.group = append(r.group, line.GroupDescendant)
		case line.Descendant > 0:
			r.other = append(r.other, line.Descendant)
		}
	}
	return r, sc.Err()
}

func refreshCalls(r recorded) int {
	n := 0
	for _, m := range r.requests {
		if params, ok := m["params"].(map[string]any); ok && m["method"] == "getAuthStatus" && params["refreshToken"] == true {
			n++
		}
	}
	return n
}

func (w *world) subst(s string) string {
	return strings.NewReplacer("$home", w.home, "$codexHome", w.codexHome, "$binDir", w.binDir).Replace(s)
}

// probe runs one probe and checks every expectation key.
func (w *world) probe(want map[string]any) []string {
	ctx := context.Background()
	if w.cancelAfter > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		time.AfterFunc(w.cancelAfter, cancel)
	}
	start := time.Now()
	st, err := codexauth.Probe(ctx, w.cfg)
	elapsed := time.Since(start)
	if err != nil {
		return []string{"probe: " + err.Error()}
	}
	var tok secret.Token
	if st.State == codexauth.StateReady {
		if tok, err = st.Creds.Token(context.Background()); err != nil {
			return []string{"ready, but Token failed: " + err.Error()}
		}
	}
	rec, err := w.read()
	if err != nil {
		return []string{"record: " + err.Error()}
	}
	desc := ""
	if st.Err != nil {
		desc = st.Err.Error()
	}
	return w.check(want, string(st.State), string(openaihttp.Classify(st.Err)), tok, st.Mode, st.Info, rec, elapsed, desc)
}

func (w *world) check(want map[string]any, state, class string, tok secret.Token, mode string, info codexauth.Info, rec recorded, elapsed time.Duration, desc string) []string {
	var p []string
	for key, v := range want {
		switch key {
		case "state":
			if state != v {
				p = append(p, fmt.Sprintf("state %s, want %v", state, v))
			}
		case "class":
			if class != v {
				p = append(p, fmt.Sprintf("class %q, want %v", class, v))
			}
		case "token":
			if exp := fmt.Sprint(fixtures.Substitute(v)); tok.Bearer() != exp {
				p = append(p, "token differs")
			}
		case "expiresAt":
			got := any(nil)
			if !tok.Expires.IsZero() {
				got = tok.Expires.Unix()
			}
			if !same(got, v) {
				p = append(p, fmt.Sprintf("expiresAt %v, want %v", got, v))
			}
		case "mode":
			if mode != v {
				p = append(p, fmt.Sprintf("mode %q, want %v", mode, v))
			}
		case "codexVersion":
			if info.CodexVersion != v {
				p = append(p, fmt.Sprintf("codexVersion %q, want %v", info.CodexVersion, v))
			}
		case "spawns":
			if !same(len(rec.spawns), v) {
				p = append(p, fmt.Sprintf("spawns %d, want %v", len(rec.spawns), v))
			}
		case "argv":
			for _, s := range rec.spawns {
				if !same(s.Argv, v) {
					p = append(p, fmt.Sprintf("argv %v, want %v", s.Argv, v))
				}
			}
		case "requests":
			p = append(p, checkRequests(rec.requests, v)...)
		case "envAbsent":
			for _, name := range strings_(v) {
				for _, s := range rec.spawns {
					if _, ok := s.Env[name]; ok {
						p = append(p, name+" reached the child")
					}
				}
			}
		case "envEquals":
			for name, val := range v.(map[string]any) {
				for _, s := range rec.spawns {
					if s.Env[name] != w.subst(fmt.Sprint(val)) {
						p = append(p, fmt.Sprintf("child %s=%q, want %q", name, s.Env[name], w.subst(fmt.Sprint(val))))
					}
				}
			}
		case "pathStartsWith":
			prefix := strings_(v)
			for i := range prefix {
				prefix[i] = w.subst(prefix[i])
			}
			for _, s := range rec.spawns {
				if got := strings.Split(s.Env["PATH"], ":"); len(got) < len(prefix) || !slices.Equal(got[:len(prefix)], prefix) {
					p = append(p, fmt.Sprintf("child PATH %q", s.Env["PATH"]))
				}
			}
		case "binDir":
			if got := filepath.Dir(info.Bin); got != w.subst(fmt.Sprint(v)) {
				p = append(p, fmt.Sprintf("binDir %q, want %q", got, w.subst(fmt.Sprint(v))))
			}
		case "childrenExited":
			for _, pid := range rec.pids {
				if syscall.Kill(pid, 0) == nil {
					p = append(p, fmt.Sprintf("child %d still alive", pid))
				}
			}
			// A member of the child's group is reparented when the child ends, so its new parent reaps it: allow it
			// a moment to disappear after the signal that ended it.
			for _, pid := range rec.group {
				deadline := time.Now().Add(time.Second)
				for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if syscall.Kill(pid, 0) == nil {
					p = append(p, fmt.Sprintf("process %d of the child's group still alive", pid))
				}
			}
		case "closeWithinMs":
			if limit := time.Duration(v.(float64)) * time.Millisecond; elapsed > limit {
				p = append(p, fmt.Sprintf("took %v, limit %v", elapsed, limit))
			}
		case "serverRequestAnswered": // with the method-not-found error
			answered := false
			for _, m := range rec.requests {
				if e, ok := m["error"].(map[string]any); ok && m["method"] == nil && m["id"] != nil && same(e["code"], -32601) {
					answered = true
				}
			}
			if !answered {
				p = append(p, "the server request got no error reply")
			}
		case "outbound": // every message the fake read, in order: a method, or "refusal:<id>" for an answer
			if got := outbound(rec.requests); !same(got, v) {
				p = append(p, fmt.Sprintf("outbound %v, want %v", got, v))
			}
		case "refreshCalls":
			if !same(refreshCalls(rec), v) {
				p = append(p, fmt.Sprintf("refreshCalls %d, want %v", refreshCalls(rec), v))
			}
		case "notInDescription":
			p = append(p, leaks(strings_(v), desc, w.logs.String())...)
		default:
			p = append(p, "unknown expectation "+key)
		}
	}
	return p
}

// outbound names each message the fake read, in order: its method, or "refusal:<id>" for an answer to a server
// request.
func outbound(got []map[string]any) []string {
	out := make([]string, 0, len(got))
	for _, m := range got {
		if method, ok := m["method"].(string); ok {
			out = append(out, method)
		} else {
			out = append(out, fmt.Sprintf("refusal:%v", m["id"]))
		}
	}
	return out
}

// checkRequests compares the requests and notifications the fake received (the client's answers to server requests
// excluded) with want.
func checkRequests(got []map[string]any, want any) []string {
	var calls []map[string]any
	for _, m := range got {
		if _, ok := m["method"]; ok {
			calls = append(calls, m)
		}
	}
	list, _ := want.([]any)
	if len(calls) != len(list) {
		var names []any
		for _, m := range calls {
			names = append(names, m["method"])
		}
		return []string{fmt.Sprintf("requests %v, want %d", names, len(list))}
	}
	var p []string
	for i, w := range list {
		wm := w.(map[string]any)
		m := calls[i]
		if m["method"] != wm["method"] {
			p = append(p, fmt.Sprintf("request %d is %v, want %v", i, m["method"], wm["method"]))
		}
		_, hasID := m["id"]
		if notification, _ := wm["notification"].(bool); notification == hasID {
			p = append(p, fmt.Sprintf("request %d: notification %v", i, !hasID))
		}
		if params, ok := wm["params"]; ok && !same(m["params"], params) {
			p = append(p, fmt.Sprintf("request %d params %v, want %v", i, m["params"], params))
		}
	}
	return p
}

// timeline runs each step on one credentials object at now + at.
func (w *world) timeline(cc credCase) []string {
	creds, err := codexauth.New(w.cfg)
	if err != nil {
		return []string{err.Error()}
	}
	base := w.now
	initial := secret.NewToken(initialToken(cc.Setup.Server), time.Time{}, "", "fixture")
	var current secret.Token
	var p []string
	for i, step := range cc.Steps {
		w.now = base.Add(time.Duration(step.At) * time.Second)
		var tok secret.Token
		switch step.Op {
		case "token":
			tok, err = creds.Token(context.Background())
		case "refreshAfter":
			stale := current
			if step.Stale == "initial" {
				stale = initial
			}
			tok, err = creds.RefreshAfter(context.Background(), stale)
		default:
			return []string{"unknown op " + step.Op}
		}
		if err == nil {
			current = tok
		}
		rec, rerr := w.read()
		if rerr != nil {
			return []string{rerr.Error()}
		}
		desc := ""
		if err != nil {
			desc = err.Error()
		}
		for _, problem := range w.check(step.Expect, string(codexauth.StateOf(err)), string(openaihttp.Classify(err)), tok, "", creds.Info(), rec, 0, desc) {
			p = append(p, fmt.Sprintf("step %d: %s", i, problem))
		}
	}
	return p
}

// initialToken is the token the scripted server first hands out: its getAuthStatus reply to includeToken without
// refreshToken.
func initialToken(server json.RawMessage) string {
	var sc struct {
		ByParams map[string][]struct {
			When  map[string]any   `json:"when"`
			Steps []map[string]any `json:"steps"`
		} `json:"byParams"`
	}
	if json.Unmarshal(server, &sc) != nil {
		return ""
	}
	for _, e := range sc.ByParams["getAuthStatus"] {
		if e.When["includeToken"] == true && e.When["refreshToken"] == false && len(e.Steps) > 0 {
			if res, ok := e.Steps[0]["result"].(map[string]any); ok {
				return fmt.Sprint(fixtures.Substitute(res["authToken"]))
			}
		}
	}
	return ""
}
