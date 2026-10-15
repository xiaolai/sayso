package codexapp

import (
	"path/filepath"
	"strings"
)

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
// repaired for bin. Nothing else is added, so no secret can reach the child's environment through this package.
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
