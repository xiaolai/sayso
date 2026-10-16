package codexauth

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

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
	dir  *pinnedDir // the open CODEX_HOME folder; nil for the default path
	path string     // the file, for the caller's guard and for diagnostics
}

// pinnedDir is an open folder descriptor. A pointer, so the zero authLocation closes nothing.
type pinnedDir struct{ fd int }

func (l authLocation) close() {
	if l.dir != nil {
		_ = unix.Close(l.dir.fd)
	}
}

// afterResolve, when set, runs between resolving CODEX_HOME and opening it. For tests.
var afterResolve func()

// authFilePath locates $CODEX_HOME/auth.json when codexHome resolves, after following links, to a folder the user
// owns, inside confine when that is set, and ~/.codex/auth.json when codexHome is unset. A set CODEX_HOME that fails
// is codex-error. The caller closes the location.
//
// The resolved path is then opened one folder at a time from the root, each relative to the folder before it and
// without following a link, and it is inside confine when one of the folders opened is confine's folder, compared by
// device and inode rather than by path: a path spelt in another case on a case-insensitive volume, or reaching the
// folder through another mount, names the same folder. The check is on the folders the read goes through, even when
// a folder of the path is replaced after resolution; a replacement with a link fails the open instead.
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
	var root *folderID
	if confine != "" {
		var st unix.Stat_t
		if err := unix.Stat(confine, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return authLocation{}, fail(StateCodexError, "the folder CODEX_HOME is confined to does not resolve", err)
		}
		root = &folderID{dev: uint64(st.Dev), ino: st.Ino}
	}
	if afterResolve != nil {
		afterResolve()
	}
	dir, inside, err := openDirNoFollow(real, root)
	if err != nil {
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not a folder, or a folder of its path changed", err)
	}
	if root != nil && !inside {
		_ = unix.Close(dir)
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is outside the folder it is confined to", nil)
	}
	// Checked on the open folder, the one every read goes through.
	var st unix.Stat_t
	if err := unix.Fstat(dir, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(dir)
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not a folder", err)
	}
	if int(st.Uid) != os.Getuid() {
		_ = unix.Close(dir)
		return authLocation{}, fail(StateCodexError, "CODEX_HOME is not owned by the user", nil)
	}
	return authLocation{dir: &pinnedDir{dir}, path: filepath.Join(real, authFileName)}, nil
}

// folderID is a folder's identity, the same at every path that reaches it.
type folderID struct{ dev, ino uint64 }

// openDirNoFollow opens the folder at the clean absolute path p one component at a time, never following a link and
// never blocking: a link, a FIFO or a file anywhere on the path fails. through reports whether one of the folders
// opened, "/" and the last included, is the folder *via; false when via is nil.
func openDirNoFollow(p string, via *folderID) (fd int, through bool, err error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return -1, false, errors.New("not a clean absolute path")
	}
	fd, err = openRetry(unix.AT_FDCWD, "/")
	if err != nil {
		return -1, false, err
	}
	is := func(fd int) (bool, error) {
		if via == nil {
			return false, nil
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return false, err
		}
		return uint64(st.Dev) == via.dev && st.Ino == via.ino, nil
	}
	if through, err = is(fd); err != nil {
		_ = unix.Close(fd)
		return -1, false, err
	}
	for name := range strings.SplitSeq(strings.TrimPrefix(p, "/"), "/") {
		if name == "" {
			continue // p is "/"
		}
		if name == "." || name == ".." {
			_ = unix.Close(fd)
			return -1, false, errors.New("the path names a relative folder")
		}
		next, err := openRetry(fd, name)
		_ = unix.Close(fd)
		if err != nil {
			return -1, false, err
		}
		fd = next
		if !through {
			if through, err = is(fd); err != nil {
				_ = unix.Close(fd)
				return -1, false, err
			}
		}
	}
	return fd, through, nil
}

// openRetry opens the folder name inside dir with dirFlags, retrying an interrupted call.
func openRetry(dir int, name string) (int, error) {
	for {
		fd, err := unix.Openat(dir, name, dirFlags, 0)
		if err != unix.EINTR {
			return fd, err
		}
	}
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

// fileFlags open auth.json: never through a link, and never blocking on a FIFO.
const fileFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

// openAuthFile opens the file without following a link and without blocking on a FIFO: inside the pinned folder,
// relative to its descriptor, when CODEX_HOME is set.
func openAuthFile(loc authLocation) (*os.File, *Error) {
	var fd int
	var err error
	for {
		if loc.dir == nil {
			fd, err = unix.Open(loc.path, fileFlags, 0)
		} else {
			fd, err = unix.Openat(loc.dir.fd, authFileName, fileFlags, 0)
		}
		if err != unix.EINTR {
			break
		}
	}
	if errors.Is(err, unix.ENOENT) {
		return nil, fail(StateNoSignInFile, "auth.json does not exist", errNoFile)
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
		return nil, fail(StateCodexError, "auth.json is a link", err) // FreeBSD reports EMLINK for O_NOFOLLOW
	}
	if err != nil {
		return nil, fail(StateCodexError, "auth.json cannot be opened", err)
	}
	return os.NewFile(uintptr(fd), loc.path), nil
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
	// Read where it lies, in buf, which is cleared on every exit: no check or lookup copies any of it.
	doc := buf[:n]
	if !wire.ValidCodex(doc) || wire.KindOf(doc, wire.Root(doc)) != '{' {
		return secret.Token{}, nil, false
	}
	tok, e = decodeAuthFile(doc)
	return tok, e, true
}

// fileModeList is every auth_mode value Codex knows (protocol/src/auth.rs).
var fileModeList = []string{
	modeChatGPT, modeAPIKey, "chatgptAuthTokens", "headers", "agentIdentity",
	"personalAccessToken", "bedrockApiKey", "bedrockAccessKeys",
}

// decodeAuthFile resolves the sign-in mode in Codex's own order (AuthDotJson::resolved_mode) and returns the ChatGPT
// token from doc, one validated JSON object. An unknown auth_mode makes the file unreadable to Codex itself, so it is
// codex-error. Keys are exact.
//
// Every lookup reads doc where it lies (wire.Member): names match as decoded bytes, the mode is compared in place
// with the modes Codex knows, and a value of the wrong kind is refused from its first byte. The access token, decoded
// into a buffer cleared once the string kept is made from it, is the one copy made of anything in the file.
func decodeAuthFile(doc []byte) (secret.Token, *Error) {
	root := wire.Root(doc)
	present := func(key string) bool {
		v, ok := wire.Member(doc, root, key)
		return ok && wire.KindOf(doc, v) != 'n'
	}
	mode := modeChatGPT
	switch {
	case present("auth_mode"):
		v, _ := wire.Member(doc, root, "auth_mode")
		known := ""
		for _, m := range fileModeList {
			if wire.StringEquals(doc, v, m) {
				known = m
				break
			}
		}
		if known == "" {
			return secret.Token{}, fail(StateCodexError, "auth.json names an unknown auth_mode", nil)
		}
		mode = known
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
	// A tokens value that is not an object holds no access token.
	tokens, ok := wire.Member(doc, root, "tokens")
	if !ok {
		return secret.Token{}, missing
	}
	a, ok := wire.Member(doc, tokens, "access_token")
	if !ok {
		return secret.Token{}, missing
	}
	buf := make([]byte, 0, a.End-a.Start)
	defer clear(buf[:cap(buf)])
	buf, ok = wire.AppendString(buf, doc, a)
	if !ok || len(buf) == 0 {
		return secret.Token{}, missing
	}
	return newToken(string(buf), "file"), nil
}
