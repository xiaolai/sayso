package codexauth

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"

	"github.com/xiaolai/sayso/go/secret"
)

const (
	// maxAuthFile is the size from which auth.json is refused.
	maxAuthFile = 64 << 10
	// tornRetryDelay is how long the one re-read after a parse failure waits for a writer to finish.
	tornRetryDelay = 100 * time.Millisecond
	authFileName   = "auth.json"
)

// authLocation is where auth.json is read: a pinned, validated CODEX_HOME folder, or, with CODEX_HOME unset, the
// default path. The folder stays open for every read, so replacing a folder of the path after validation (with a
// link elsewhere, say) cannot redirect a later read.
type authLocation struct {
	dir  *os.Root // nil for the default path
	path string   // the file, for the caller's guard and for diagnostics
}

func (l authLocation) close() {
	if l.dir != nil {
		_ = l.dir.Close()
	}
}

// authFilePath locates $CODEX_HOME/auth.json when codexHome resolves, after following links, to a folder the user
// owns, inside confine when that is set, and ~/.codex/auth.json when codexHome is unset. A set CODEX_HOME that fails
// is codex-error. The caller closes the location.
func authFilePath(codexHome, home, confine string) (authLocation, *Error) {
	if codexHome == "" {
		return authLocation{path: filepath.Join(home, ".codex", authFileName)}, nil
	}
	if strings.ContainsRune(codexHome, 0) || strings.ContainsRune(confine, 0) {
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not a valid path", nil)
	}
	if !filepath.IsAbs(codexHome) {
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not an absolute path", nil)
	}
	real, err := filepath.EvalSymlinks(codexHome)
	if err != nil {
		return authLocation{}, fail(StateCodexError, "CODEX_HOME does not resolve", err)
	}
	var dir *os.Root
	if confine != "" {
		root, err := filepath.EvalSymlinks(confine)
		if err != nil {
			return authLocation{}, fail(StateCodexError, "the folder CODEX_HOME is confined to does not resolve", err)
		}
		rel, err := filepath.Rel(root, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return authLocation{}, fail(StateCodexError, "CODEX_HOME is outside the folder it is confined to", nil)
		}
		// Opened through the confining folder, which refuses any path that leaves it, even one changed since.
		top, err := os.OpenRoot(root)
		if err != nil {
			return authLocation{}, fail(StateCodexError, "the folder CODEX_HOME is confined to does not resolve", err)
		}
		dir, err = top.OpenRoot(rel)
		_ = top.Close()
		if err != nil {
			return authLocation{}, fail(StateCodexError, "CODEX_HOME is not a folder inside the folder it is confined to", err)
		}
	} else if dir, err = os.OpenRoot(real); err != nil {
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not a folder", err)
	}
	// Checked on the open folder, the one every read goes through.
	fi, err := dir.Stat(".")
	if err != nil || !fi.IsDir() {
		_ = dir.Close()
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not a folder", err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		_ = dir.Close()
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not owned by the user", nil)
	}
	return authLocation{dir: dir, path: filepath.Join(real, authFileName)}, nil
}

// errNoFile marks a missing auth.json.
var errNoFile = errors.New("auth.json does not exist")

// readAuthFile reads the file at loc and returns the ChatGPT token it holds. A missing file is no-sign-in-file
// wrapping errNoFile; a parse failure is retried once after onRetry and a short delay.
func readAuthFile(loc authLocation, onRetry func()) (secret.Token, *Error) {
	tok, e, parsed := readOnce(loc)
	if parsed {
		return tok, e
	}
	if onRetry != nil {
		onRetry()
	}
	time.Sleep(tornRetryDelay)
	tok, e, parsed = readOnce(loc)
	if !parsed {
		return secret.Token{}, fail(StateCodexError, "auth.json is not a valid sign-in file", nil)
	}
	return tok, e
}

// openAuthFile opens the file without following a link and without blocking on a FIFO.
func openAuthFile(loc authLocation) (*os.File, *Error) {
	if loc.dir == nil {
		fd, err := syscall.Open(loc.path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if errors.Is(err, syscall.ENOENT) {
			return nil, fail(StateNoSignInFile, "auth.json does not exist", errNoFile)
		}
		if err != nil {
			return nil, fail(StateCodexError, "auth.json cannot be opened", err)
		}
		return os.NewFile(uintptr(fd), loc.path), nil
	}
	// os.Root follows a link that stays inside the folder, so the file is examined first and the opened file must be
	// that same regular file: an inode cannot be both a link and the file it was checked to be.
	li, err := loc.dir.Lstat(authFileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fail(StateNoSignInFile, "auth.json does not exist", errNoFile)
	}
	if err != nil {
		return nil, fail(StateCodexError, "auth.json cannot be opened", err)
	}
	if !li.Mode().IsRegular() {
		return nil, fail(StateCodexError, "auth.json is not a regular file", nil)
	}
	f, err := loc.dir.OpenFile(authFileName, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fail(StateNoSignInFile, "auth.json does not exist", errNoFile)
	}
	if err != nil {
		return nil, fail(StateCodexError, "auth.json cannot be opened", err)
	}
	if fi, err := f.Stat(); err != nil || !os.SameFile(li, fi) {
		_ = f.Close()
		return nil, fail(StateCodexError, "auth.json changed while it was opened", err)
	}
	return f, nil
}

// readOnce opens the file, checks the open descriptor, and decodes the bytes read from it. parsed is false only
// when the content did not decode.
func readOnce(loc authLocation) (tok secret.Token, e *Error, parsed bool) {
	f, e := openAuthFile(loc)
	if e != nil {
		return secret.Token{}, e, true
	}
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
	// One buffer, never grown, so wiping it wipes every raw byte read: the bytes hold every field, any API key.
	buf := make([]byte, maxAuthFile)
	defer clear(buf)
	n := 0
	for n < len(buf) {
		k, err := f.Read(buf[n:])
		n += k
		if err == io.EOF {
			break
		}
		if err != nil {
			return secret.Token{}, fail(StateCodexError, "auth.json cannot be read", err), true
		}
	}
	if n >= maxAuthFile {
		return secret.Token{}, fail(StateCodexError, "auth.json is 64 KiB or larger", nil), true
	}
	decodeMu.Lock()
	defer func() {
		for _, b := range decoded {
			clear(b)
		}
		decoded = nil
		decodeMu.Unlock()
	}()
	var doc map[string]wipedRaw
	if !wire.Valid(buf[:n]) || json.Unmarshal(buf[:n], &doc) != nil || doc == nil {
		return secret.Token{}, nil, false
	}
	tok, e = decodeAuthFile(doc)
	return tok, e, true
}

// Every value decoded from auth.json is a wipedRaw, recorded in decoded so it is wiped after the read, including a
// value a duplicate key replaced. decodeMu serialises reads, which are rare.
var (
	decodeMu sync.Mutex
	decoded  [][]byte
)

type wipedRaw []byte

func (w *wipedRaw) UnmarshalJSON(b []byte) error {
	c := append([]byte(nil), b...)
	decoded = append(decoded, c)
	*w = c
	return nil
}

// fileModes are the auth_mode values Codex knows (protocol/src/auth.rs), other than chatgpt and apikey.
var fileModes = map[string]bool{
	"chatgptAuthTokens": true, "headers": true, "agentIdentity": true,
	"personalAccessToken": true, "bedrockApiKey": true, "bedrockAccessKeys": true,
}

// decodeAuthFile resolves the sign-in mode in Codex's own order (AuthDotJson::resolved_mode) and returns the ChatGPT
// token. An unknown auth_mode makes the file unreadable to Codex itself, so it is codex-error. Keys are exact.
func decodeAuthFile(doc map[string]wipedRaw) (secret.Token, *Error) {
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
	missing := fail(StateCodexError, "auth.json has a ChatGPT sign-in without an access token", nil)
	var tokens map[string]wipedRaw
	if !present("tokens") || json.Unmarshal(doc["tokens"], &tokens) != nil {
		return secret.Token{}, missing
	}
	var access string
	if raw, ok := tokens["access_token"]; !ok || json.Unmarshal(raw, &access) != nil || access == "" {
		return secret.Token{}, missing
	}
	return newToken(access, "file"), nil
}
