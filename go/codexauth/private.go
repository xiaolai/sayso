package codexauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xiaolai/sayso/go/secret"
)

// privateFileFlags create auth.json in a private home: write-only, new (an entry of that name already there, a link
// included, is refused rather than replaced or followed), and closed on exec.
const privateFileFlags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC

// WritePrivateSignIn writes auth.json into codexHome, a folder made for a Codex of the caller's own, from tok, a token
// a fetch or refresh returned (spec/credentials.md, AUTH-08). The file signs that Codex in to the ChatGPT account the
// token names, with no refresh token, and is stamped as refreshed at now. The user's own auth.json is never read.
//
// codexHome must be an absolute path to a folder the user owns that grants nothing to anyone else (no group or other
// permission bits); a link to one is followed once, and the folder is then opened along the resolved path without
// following a link. auth.json must not exist there. Every failure is a *Error with StateCodexError, and leaves no
// auth.json behind. The bytes are built in a buffer of their own, cleared once written.
//
// The token's freshness is the caller's: one the caller fetched with the margin its Codex needs, not one already
// expired at now, which is refused.
func WritePrivateSignIn(codexHome string, tok secret.Token, now time.Time) error {
	bearer := tok.Bearer()
	if !jwtShaped(bearer) {
		return fail(StateCodexError, "the token is not a JWT, so a private Codex cannot read it", nil)
	}
	account, ok := accountClaim(bearer)
	if !ok {
		return fail(StateCodexError, "the token names no ChatGPT account", nil)
	}
	if exp := jwtExpiry(bearer); !exp.IsZero() && !exp.After(now) {
		return fail(StateCodexError, "the token has expired", nil)
	}
	dir, e := openPrivateHome(codexHome)
	if e != nil {
		return e
	}
	defer func() { _ = unix.Close(dir) }()

	stamp := now.UTC().Format("2006-01-02T15:04:05Z")
	parts := []string{`{"auth_mode":"chatgpt","last_refresh":"`, stamp, `","tokens":{"id_token":"`, bearer,
		`","access_token":"`, bearer, `","refresh_token":"","account_id":"`, account, `"}}`}
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	// One buffer of the exact size, never grown, so clearing it clears every byte of the token written.
	buf := make([]byte, 0, n)
	defer clear(buf[:cap(buf)])
	for _, p := range parts {
		buf = append(buf, p...)
	}
	if e := writeNew(dir, buf); e != nil {
		return e
	}
	return nil
}

// jwtShaped reports whether s is three base64url segments joined by dots, each non-empty: a token Codex can read as
// a JWT, and one written into JSON as it is, with nothing to escape.
func jwtShaped(s string) bool {
	segments := strings.Split(s, ".")
	if len(segments) != 3 {
		return false
	}
	for _, seg := range segments {
		if seg == "" {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if base64URL(seg[i]) < 0 {
				return false
			}
		}
	}
	return true
}

// openPrivateHome resolves codexHome once and opens the folder along the resolved path without following a link,
// then checks the open folder: a folder, owned by the user, granting nothing to anyone else.
func openPrivateHome(codexHome string) (int, *Error) {
	if codexHome == "" || strings.ContainsRune(codexHome, 0) || !filepath.IsAbs(codexHome) {
		return -1, fail(StateCodexError, "the private Codex home is not an absolute path", nil)
	}
	real, err := filepath.EvalSymlinks(codexHome)
	if err != nil {
		return -1, fail(StateCodexError, "the private Codex home does not resolve", err)
	}
	dir, _, err := openDirNoFollow(real, nil)
	if err != nil {
		return -1, fail(StateCodexError, "the private Codex home is not a folder, or a folder of its path changed", err)
	}
	var st unix.Stat_t
	switch err := unix.Fstat(dir, &st); {
	case err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR:
		_ = unix.Close(dir)
		return -1, fail(StateCodexError, "the private Codex home is not a folder", err)
	case int(st.Uid) != os.Getuid():
		_ = unix.Close(dir)
		return -1, fail(StateCodexError, "the private Codex home is not owned by the user", nil)
	case uint32(st.Mode)&0o077 != 0:
		_ = unix.Close(dir)
		return -1, fail(StateCodexError, "the private Codex home grants access to other users", nil)
	}
	return dir, nil
}

// writeNew creates auth.json in dir, mode 0600 whatever the umask, and writes data to it in full; on any failure the
// file it created is removed.
func writeNew(dir int, data []byte) *Error {
	var fd int
	var err error
	for {
		if fd, err = unix.Openat(dir, authFileName, privateFileFlags, 0o600); err != unix.EINTR {
			break
		}
	}
	if errors.Is(err, unix.EEXIST) {
		return fail(StateCodexError, "the private Codex home already holds an auth.json", nil)
	}
	if err != nil {
		return fail(StateCodexError, "auth.json cannot be created in the private Codex home", err)
	}
	created := func(e *Error) *Error {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(dir, authFileName, 0)
		return e
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return created(fail(StateCodexError, "auth.json cannot be made private", err))
	}
	for written := 0; written < len(data); {
		k, err := unix.Write(fd, data[written:])
		if err == unix.EINTR {
			continue
		}
		if err == nil && k == 0 {
			err = unix.EIO // a regular file never takes nothing; never spin on it
		}
		if err != nil {
			return created(fail(StateCodexError, "auth.json cannot be written", err))
		}
		written += k
	}
	if err := unix.Close(fd); err != nil {
		_ = unix.Unlinkat(dir, authFileName, 0)
		return fail(StateCodexError, "auth.json cannot be written", err)
	}
	return nil
}
