package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/codexauth"
	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

// privateSignIn writes a private sign-in (AUTH-08) into the folder the case sets up, and checks every expectation
// key against what the folder holds afterwards.
func privateSignIn(t *testing.T, cc credCase) []string {
	s := cc.Setup.PrivateSignIn
	var token any
	if err := json.Unmarshal(s.Token, &token); err != nil {
		return []string{"setup.privateSignIn.token: " + err.Error()}
	}
	bearer, ok := fixtures.Substitute(token).(string)
	if !ok {
		return []string{"setup.privateSignIn.token is not a string or a $jwt"}
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		return []string{err.Error()}
	}
	folder, listed, err := privateFolder(root, s.Folder)
	if err != nil {
		return []string{"setup: " + err.Error()}
	}
	now := time.Unix(cmp(cc.Setup.Now, 1800000000), 0)
	err = codexauth.WritePrivateSignIn(folder, secret.NewToken(bearer, time.Time{}, "", "fixture"), now)

	var p []string
	desc := ""
	if err != nil {
		desc = err.Error()
	}
	for key, v := range cc.Expect {
		switch key {
		case "state":
			if got := string(codexauth.StateOf(err)); got != v {
				p = append(p, fmt.Sprintf("state %s, want %v", got, v))
			}
		case "class":
			if got := string(openaihttp.Classify(err)); got != v {
				p = append(p, fmt.Sprintf("class %q, want %v", got, v))
			}
		case "signIn":
			want := strings.ReplaceAll(fmt.Sprint(v), "$token", bearer)
			if got, err := regularContent(filepath.Join(listed, "auth.json")); err != nil {
				p = append(p, "signIn: "+err.Error())
			} else if got != want {
				p = append(p, "signIn differs") // never printed: it holds the token
			}
		case "fileMode":
			if fi, err := os.Lstat(filepath.Join(listed, "auth.json")); err != nil {
				p = append(p, "fileMode: "+err.Error())
			} else if got := fmt.Sprintf("%04o", fi.Mode().Perm()); got != v {
				p = append(p, fmt.Sprintf("fileMode %s, want %v", got, v))
			}
		case "entries":
			got, err := entries(listed)
			if err != nil {
				p = append(p, "entries: "+err.Error())
			} else if !same(got, v) {
				p = append(p, fmt.Sprintf("entries %v, want %v", got, v))
			}
		case "elsewhere":
			if got, err := regularContent(filepath.Join(root, "elsewhere")); err != nil || got != v {
				p = append(p, fmt.Sprintf("elsewhere %q (%v), want %v", got, err, v))
			}
		case "notInDescription":
			var notIn []string
			for _, x := range v.([]any) {
				notIn = append(notIn, fmt.Sprint(x))
			}
			p = append(p, leaks(notIn, desc)...)
		default:
			p = append(p, "unknown expectation "+key)
		}
	}
	if err != nil && !errors.As(err, new(*codexauth.Error)) {
		p = append(p, "the error is not a *codexauth.Error")
	}
	return p
}

// privateFolder sets up the folder a private sign-in case names, under root: it returns the path passed to
// WritePrivateSignIn and the folder whose entries are compared afterwards (the one a link names).
func privateFolder(root, kind string) (arg, listed string, err error) {
	arg = filepath.Join(root, "codex")
	mk := func(mode os.FileMode) error {
		if err := os.Mkdir(arg, 0o700); err != nil {
			return err
		}
		return os.Chmod(arg, mode) // whatever the umask
	}
	switch kind {
	case "private", "taken", "taken-link":
		err = mk(0o700)
	case "open":
		err = mk(0o755)
	case "searchable":
		err = mk(0o711)
	case "link":
		real := filepath.Join(root, "real")
		if err = os.Mkdir(real, 0o700); err == nil {
			err = os.Symlink(real, arg)
		}
		return arg, real, err
	case "missing":
	case "file":
		err = os.WriteFile(arg, []byte("{}"), 0o600)
	case "relative":
		return "relative/codex", arg, nil
	default:
		return "", "", fmt.Errorf("unknown folder %q", kind)
	}
	if err != nil {
		return "", "", err
	}
	switch kind {
	case "taken":
		err = os.WriteFile(filepath.Join(arg, "auth.json"), []byte("{}"), 0o600)
	case "taken-link":
		elsewhere := filepath.Join(root, "elsewhere")
		if err = os.WriteFile(elsewhere, []byte("{}"), 0o600); err == nil {
			err = os.Symlink(elsewhere, filepath.Join(arg, "auth.json"))
		}
	}
	return arg, arg, err
}

// regularContent is the content of the regular file at path, read without following a link.
func regularContent(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// entries names each entry of dir with its kind: file, link, folder or other.
func entries(dir string) (map[string]string, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range list {
		switch t := e.Type(); {
		case t.IsRegular():
			out[e.Name()] = "file"
		case t&os.ModeSymlink != 0:
			out[e.Name()] = "link"
		case t.IsDir():
			out[e.Name()] = "folder"
		default:
			out[e.Name()] = "other"
		}
	}
	return out, nil
}
