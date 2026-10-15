package codexauth

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xiaolai/sayso/go/secret"
)

const (
	// maxAuthFile is the size from which auth.json is refused.
	maxAuthFile = 64 << 10
	// tornRetryDelay is how long the one re-read after a parse failure waits for a writer to finish.
	tornRetryDelay = 100 * time.Millisecond
)

// authFilePath returns $CODEX_HOME/auth.json when codexHome resolves, after following links, to a folder the user
// owns inside home, and ~/.codex/auth.json when codexHome is unset. A set CODEX_HOME that fails is codex-error.
func authFilePath(codexHome, home string) (string, *Error) {
	if codexHome == "" {
		return filepath.Join(home, ".codex", "auth.json"), nil
	}
	if !filepath.IsAbs(codexHome) {
		return "", fail(StateCodexError, "CODEX_HOME is not an absolute path", nil)
	}
	real, err := filepath.EvalSymlinks(codexHome)
	if err != nil {
		return "", fail(StateCodexError, "CODEX_HOME does not resolve", err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fail(StateCodexError, "the home folder does not resolve", err)
	}
	rel, err := filepath.Rel(realHome, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fail(StateCodexError, "CODEX_HOME is outside the home folder", nil)
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return "", fail(StateCodexError, "CODEX_HOME is not a folder", err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return "", fail(StateCodexError, "CODEX_HOME is not owned by the user", nil)
	}
	return filepath.Join(real, "auth.json"), nil
}

// errNoFile marks a missing auth.json.
var errNoFile = errors.New("auth.json does not exist")

// readAuthFile reads path and returns the ChatGPT token it holds. A missing file is no-sign-in-file wrapping
// errNoFile; a parse failure is retried once after onRetry and a short delay.
func readAuthFile(path string, onRetry func()) (secret.Token, *Error) {
	tok, e, parsed := readOnce(path)
	if parsed {
		return tok, e
	}
	if onRetry != nil {
		onRetry()
	}
	time.Sleep(tornRetryDelay)
	tok, e, parsed = readOnce(path)
	if !parsed {
		return secret.Token{}, fail(StateCodexError, "auth.json is not a valid sign-in file", nil)
	}
	return tok, e
}

// readOnce opens path without following a link and without blocking on a FIFO, checks the open descriptor, and
// decodes the bytes read from it. parsed is false only when the content did not decode.
func readOnce(path string) (tok secret.Token, e *Error, parsed bool) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return secret.Token{}, fail(StateNoSignInFile, "auth.json does not exist", errNoFile), true
	}
	if err != nil {
		return secret.Token{}, fail(StateCodexError, "auth.json cannot be opened", err), true
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return secret.Token{}, fail(StateCodexError, "auth.json cannot be inspected", err), true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return secret.Token{}, fail(StateCodexError, "auth.json is not a regular file", nil), true
	case !ok || int(st.Uid) != os.Getuid():
		return secret.Token{}, fail(StateCodexError, "auth.json is not owned by the user", nil), true
	case fi.Size() >= maxAuthFile:
		return secret.Token{}, fail(StateCodexError, "auth.json is 64 KiB or larger", nil), true
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxAuthFile))
	defer clear(buf) // the raw bytes hold every field, including any API key
	if err != nil {
		return secret.Token{}, fail(StateCodexError, "auth.json cannot be read", err), true
	}
	if len(buf) >= maxAuthFile {
		return secret.Token{}, fail(StateCodexError, "auth.json is 64 KiB or larger", nil), true
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(buf, &doc) != nil || doc == nil {
		return secret.Token{}, nil, false
	}
	defer func() {
		for _, v := range doc {
			clear(v)
		}
	}()
	tok, e = decodeAuthFile(doc)
	return tok, e, true
}

// fileModes are the auth_mode values Codex knows (protocol/src/auth.rs), other than chatgpt and apikey.
var fileModes = map[string]bool{
	"chatgptAuthTokens": true, "headers": true, "agentIdentity": true,
	"personalAccessToken": true, "bedrockApiKey": true, "bedrockAccessKeys": true,
}

// decodeAuthFile resolves the sign-in mode in Codex's own order (AuthDotJson::resolved_mode) and returns the ChatGPT
// token. An unknown auth_mode makes the file unreadable to Codex itself, so it is codex-error.
func decodeAuthFile(doc map[string]json.RawMessage) (secret.Token, *Error) {
	present := func(key string) bool { v, ok := doc[key]; return ok && string(v) != "null" }
	mode := modeChatGPT
	switch {
	case present("auth_mode"):
		var m string
		if json.Unmarshal(doc["auth_mode"], &m) != nil || (m != modeChatGPT && m != modeAPIKey && !fileModes[m]) {
			return secret.Token{}, fail(StateCodexError, "auth.json names an unknown auth_mode", nil)
		}
		mode = m
	case present("personal_access_token"):
		mode = "personalAccessToken"
	case present("bedrock_api_key"):
		mode = "bedrockApiKey"
	case present("bedrock_access_keys"):
		mode = "bedrockAccessKeys"
	case present("OPENAI_API_KEY"):
		mode = modeAPIKey
	}
	if e := classifyMode(mode); e != nil {
		return secret.Token{}, e
	}
	var tokens *struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(doc["tokens"], &tokens) != nil || tokens == nil || tokens.AccessToken == "" {
		return secret.Token{}, fail(StateCodexError, "auth.json has a ChatGPT sign-in without an access token", nil)
	}
	return newToken(tokens.AccessToken, "file"), nil
}
