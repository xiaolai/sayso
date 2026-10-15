// Package codexauth obtains the ChatGPT access token Codex holds for this machine: from `codex app-server` first, from
// auth.json only when no Codex runs or Codex says its store is a file (spec/credentials.md). Only a ChatGPT sign-in
// is accepted. The package has no user-facing strings: callers map a State to their own text.
package codexauth

import (
	"errors"
	"strings"
	"unicode"

	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

// State is the outcome of a probe. Exactly one applies.
type State string

// States (spec/credentials.md).
const (
	StateReady                     State = "ready"
	StateNoCodex                   State = "no-codex"
	StateCodexTooOld               State = "codex-too-old"
	StateNotSignedIn               State = "not-signed-in"
	StateNoSignInFile              State = "no-sign-in-file"
	StateAPIKey                    State = "api-key"
	StateUnsupported               State = "unsupported-auth"
	StateExpired                   State = "expired"
	StateCodexError                State = "codex-error"
	StateAppServerAuthUnavailable  State = "app-server-auth-unavailable"
	StateProviderWithoutOpenAIAuth State = "provider-without-openai-auth"
)

// MinCodexVersion is the oldest Codex supported.
const MinCodexVersion = "0.162.0"

// Error is a failure to obtain a usable token. It unwraps to an *openaihttp.Error, so openaihttp.Classify gives
// auth-expired for StateExpired, codex-error for Codex and file failures, and auth-mode for the sign-in states.
type Error struct {
	State        State
	Mode         string // the reported sign-in mode (api-key, unsupported-auth), scrubbed and bounded; "" otherwise
	CodexVersion string // "" when unknown
	Err          error
}

func (e *Error) Error() string {
	s := "codexauth: " + string(e.State)
	if e.Mode != "" {
		s += " (mode " + e.Mode + ")"
	}
	if e.Err != nil {
		s += ": " + secret.Scrub(e.Err.Error())
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// StateOf returns the state an error from this package carries: StateReady for nil, StateCodexError for an error
// from elsewhere.
func StateOf(err error) State {
	if err == nil {
		return StateReady
	}
	var e *Error
	if errors.As(err, &e) {
		return e.State
	}
	return StateCodexError
}

// fail builds an *Error whose cause has the class that suits state.
func fail(state State, msg string, cause error) *Error {
	class := openaihttp.ClassAuthMode
	switch state {
	case StateExpired:
		class = openaihttp.ClassAuthExpired
	case StateCodexError, StateNoCodex, StateCodexTooOld, StateAppServerAuthUnavailable:
		class = openaihttp.ClassCodexError
	}
	return &Error{State: state, Err: &openaihttp.Error{Class: class, Message: msg, Err: cause}}
}

const (
	modeChatGPT = "chatgpt"
	modeAPIKey  = "apikey"
	maxModeLen  = 64
)

// classifyMode returns nil for chatgpt and the state's error for any other mode; "" is not signed in.
func classifyMode(mode string) *Error {
	switch mode {
	case modeChatGPT:
		return nil
	case "":
		return fail(StateNotSignedIn, "Codex is not signed in", nil)
	case modeAPIKey:
		e := fail(StateAPIKey, "Codex is signed in with an API key", nil)
		e.Mode = modeAPIKey
		return e
	}
	e := fail(StateUnsupported, "Codex is signed in with an unsupported mode", nil)
	e.Mode = cleanMode(mode)
	return e
}

// cleanMode makes a reported mode safe to print: scrubbed, printable, at most 64 characters.
func cleanMode(m string) string {
	m = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, secret.Scrub(m))
	if r := []rune(m); len(r) > maxModeLen {
		m = string(r[:maxModeLen])
	}
	return m
}
