package codexapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
)

// Env is the environment handed to the child, which may hold credentials. The variables live in a closure: fmt
// prints a func as its address and reflection can neither read a closure's variables nor call a func reached through
// an unexported field, so a Config held anywhere, an unexported wrapper field included, cannot show them. The zero
// Env inherits os.Environ() when the child starts.
type Env struct{ get func() []string }

// EnvOf holds a copy of kv. EnvOf(nil) is the zero Env, which inherits; an empty, non-nil kv is an empty environment.
func EnvOf(kv []string) Env {
	if kv == nil {
		return Env{}
	}
	held := append([]string{}, kv...)
	return Env{get: func() []string { return slices.Clone(held) }}
}

// IsZero reports the zero Env, which inherits os.Environ().
func (e Env) IsZero() bool { return e.get == nil }

// List returns a copy of the variables; nil for the zero Env.
func (e Env) List() []string {
	if e.get == nil {
		return nil
	}
	return e.get()
}

// String never shows a variable.
func (e Env) String() string {
	if e.get == nil {
		return "codexapp.Env{inherited}"
	}
	return "codexapp.Env{redacted}"
}

// GoString redacts like String.
func (e Env) GoString() string { return e.String() }

// Format redacts like String for every verb.
func (e Env) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(e.String())) }

// MarshalJSON encodes the redacted form.
func (e Env) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

// LogValue logs the redacted form.
func (e Env) LogValue() slog.Value { return slog.StringValue(e.String()) }

// removedEnv are the variables that override Codex's own authentication or its token endpoints (upstream
// login/src/auth/manager.rs). They never reach the child; names compare case-insensitively.
var removedEnv = []string{
	"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_APP_SERVER_LOGIN_CLIENT_ID",
	"CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE",
}

// repairPath is prepended to the inherited PATH, after the executable's folder, so an npm-installed codex
// (`#!/usr/bin/env node`) finds its interpreter when the app was launched from the Finder.
const repairPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

// ChildEnv is env minus the authentication overrides, with CODEX_HOME replaced when codexHome is set and PATH
// repaired for bin. Only those overrides are removed: every other inherited variable, credentials of other services
// included, is forwarded as it is. Nothing else is added.
func ChildEnv(env []string, bin, codexHome string) []string {
	out := make([]string, 0, len(env)+2)
	inherited := ""
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case removed(name):
			continue
		case name == "PATH":
			inherited = value
			continue
		case codexHome != "" && strings.EqualFold(name, "CODEX_HOME"):
			continue
		}
		out = append(out, kv)
	}
	path := filepath.Dir(bin) + ":" + repairPath
	if inherited != "" {
		path += ":" + inherited
	}
	out = append(out, "PATH="+path)
	if codexHome != "" {
		out = append(out, "CODEX_HOME="+codexHome)
	}
	return out
}

func removed(name string) bool {
	for _, r := range removedEnv {
		if strings.EqualFold(name, r) {
			return true
		}
	}
	return false
}
