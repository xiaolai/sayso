package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/codexapp"
	"github.com/xiaolai/sayso/go/codexauth"
	"github.com/xiaolai/sayso/go/secret"
)

// clientSetup drives the app-server client itself rather than a credentials object (spec/fixtures.md).
type clientSetup struct {
	Handler         string `json:"handler"` // "" (none), "record", or "block" (never returns until the case ends)
	Dir             string `json:"dir"`     // "" (none), "work", "relative", "missing" or "file"
	ExperimentalAPI bool   `json:"experimentalAPI"`
	Calls           []struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	} `json:"calls"`
}

// clientKeys are the expectations client checks itself; the rest go to check.
var clientKeys = map[string]bool{"startFails": true, "calls": true, "events": true, "eventCount": true, "eventSequence": true}

func (w *world) client(t *testing.T, s *clientSetup, want map[string]any) []string {
	var (
		mu     sync.Mutex
		events []codexapp.Event
	)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	cfg := codexapp.Config{Bin: filepath.Join(w.binDir, "codex"), Env: w.cfg.Env, Timeout: w.cfg.Timeout,
		Logger: w.cfg.Logger, Guard: w.cfg.Guard, ExperimentalAPI: s.ExperimentalAPI}
	switch s.Handler {
	case "":
	case "record":
		cfg.OnEvent = func(e codexapp.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
	case "block":
		cfg.OnEvent = func(codexapp.Event) { <-block }
	default:
		return []string{"unknown handler " + s.Handler}
	}
	work := filepath.Join(w.root, "work")
	switch s.Dir {
	case "":
	case "work":
		if err := os.Mkdir(work, 0o700); err != nil {
			return []string{err.Error()}
		}
		cfg.Dir = work
	case "relative":
		cfg.Dir = "work"
	case "missing":
		cfg.Dir = filepath.Join(w.root, "missing")
	case "file":
		cfg.Dir = filepath.Join(w.root, "file")
		if err := os.WriteFile(cfg.Dir, nil, 0o600); err != nil {
			return []string{err.Error()}
		}
	default:
		return []string{"unknown dir " + s.Dir}
	}

	var p []string
	ctx := context.Background()
	c, err := codexapp.Start(ctx, cfg)
	if fails, ok := want["startFails"]; ok && fails != (err != nil) {
		p = append(p, fmt.Sprintf("start failed: %v, want %v", err != nil, fails))
	} else if !ok && err != nil {
		p = append(p, "start: "+err.Error())
	}
	var outcomes []string
	if c != nil {
		for _, call := range s.Calls {
			var params any // a fixture without params sends Call's default
			if call.Params != nil {
				params = call.Params
			}
			if err := c.Call(ctx, call.Method, params, nil); err != nil {
				outcomes = append(outcomes, "error")
			} else {
				outcomes = append(outcomes, "ok")
			}
		}
		// Events are delivered from a goroutine of their own: wait for the expected number, then a moment for any
		// that should not come.
		n := 0
		if list, ok := want["events"].([]any); ok {
			n = len(list)
		}
		if v, ok := want["eventCount"].(float64); ok {
			n = int(v)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			got := len(events)
			mu.Unlock()
			if got >= n {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		_ = c.Close()
	}
	mu.Lock()
	got := slices.Clone(events)
	mu.Unlock()

	callerCwd, _ := os.Getwd()
	if r, err := filepath.EvalSymlinks(callerCwd); err == nil {
		callerCwd = r
	}
	expand := strings.NewReplacer("$work", work, "$callerCwd", callerCwd)
	rest := map[string]any{}
	for key, v := range want {
		switch key {
		case "startFails":
		case "calls":
			if !same(outcomes, v) {
				p = append(p, fmt.Sprintf("calls %v, want %v", outcomes, v))
			}
		case "events":
			list := v.([]any)
			if len(got) != len(list) {
				p = append(p, fmt.Sprintf("%d events, want %d: %s", len(got), len(list), methods(got)))
				break
			}
			for i, e := range list {
				p = append(p, checkClientEvent(i, got[i], e.(map[string]any), expand)...)
			}
		case "eventCount":
			if !same(len(got), v) {
				p = append(p, fmt.Sprintf("%d events, want %v", len(got), v))
			}
		case "eventSequence":
			for i, e := range got {
				var params map[string]any
				if err := json.Unmarshal(e.Params, &params); err != nil || !same(params[v.(string)], i) {
					p = append(p, fmt.Sprintf("event %d out of order", i))
					break
				}
			}
		default:
			if !clientKeys[key] {
				rest[key] = v
			}
		}
	}
	rec, err := w.read()
	if err != nil {
		return append(p, "record: "+err.Error())
	}
	return append(p, w.check(rest, "", secret.Token{}, "", codexauth.Info{}, rec, 0, "")...)
}

func checkClientEvent(i int, got codexapp.Event, want map[string]any, expand *strings.Replacer) []string {
	var p []string
	if got.Method != want["method"] {
		p = append(p, fmt.Sprintf("event %d is %s, want %v", i, got.Method, want["method"]))
	}
	if req, _ := want["serverRequest"].(bool); got.Request != req {
		p = append(p, fmt.Sprintf("event %d: server request %v, want %v", i, got.Request, req))
	}
	if none, _ := want["noParams"].(bool); none && got.Params != nil {
		p = append(p, fmt.Sprintf("event %d has params", i))
	}
	if wantParams, ok := want["params"]; ok {
		var params any
		if got.Params == nil || json.Unmarshal(got.Params, &params) != nil || !same(params, expandAll(wantParams, expand)) {
			p = append(p, fmt.Sprintf("event %d params differ", i))
		}
	}
	return p
}

// expandAll replaces the path placeholders in every string of v.
func expandAll(v any, r *strings.Replacer) any {
	switch t := v.(type) {
	case string:
		return r.Replace(t)
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			out[k] = expandAll(x, r)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = expandAll(x, r)
		}
		return out
	}
	return v
}

func methods(events []codexapp.Event) string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Method
	}
	return strings.Join(names, ",")
}
