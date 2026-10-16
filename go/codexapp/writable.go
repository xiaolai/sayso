package codexapp

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	modeOtherWrite = 0o002
	modeSticky     = 0o1000
	maxLinkHops    = 40 // as the system's own limit (ELOOP)
)

// changeableByOthers reports whether every user of the machine could change what the absolute path p runs: the file
// it names, or a folder whose entries are read on the way to it, is writable by everyone. Links are followed as the
// system follows them, and the folder holding each link counts, since replacing the link changes the file. A sticky
// folder (/tmp) owned by this user or by root does not count, as long as the entry read there is owned by one of
// them too: there only an entry's owner may replace it. Anything
// that cannot be read counts as changeable. Group write, ACLs, owners and the interpreter a script names are not
// judged (spec/credentials.md, executable): this refuses an installation anyone can change, not one whose owners
// the user has not vetted.
func changeableByOthers(p string) bool {
	uid := uint32(os.Getuid())
	trusted := func(owner uint32) bool { return owner == uid || owner == 0 }
	// open reports a folder everyone may write to; sticky, that it is sticky and its owner trusted, so that only an
	// entry's owner may replace the entry, which must then be trusted too.
	open := func(dir string) (open, sticky bool) {
		var st syscall.Stat_t
		if syscall.Lstat(dir, &st) != nil {
			return true, false
		}
		mode := uint32(st.Mode)
		return mode&modeOtherWrite != 0, mode&modeSticky != 0 && trusted(st.Uid)
	}
	rest := components(p)
	dir := "/"
	for hops := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		if name == ".." {
			dir = filepath.Dir(dir) // dir holds no link, so its parent is the one the system walks to
			continue
		}
		openDir, sticky := open(dir)
		if openDir && !sticky {
			return true
		}
		next := filepath.Join(dir, name)
		var st syscall.Stat_t
		if syscall.Lstat(next, &st) != nil || openDir && !trusted(st.Uid) {
			return true
		}
		if st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			target, err := os.Readlink(next)
			if hops++; err != nil || hops > maxLinkHops {
				return true
			}
			if filepath.IsAbs(target) {
				dir = "/"
			}
			rest = append(components(target), rest...)
			continue
		}
		if len(rest) == 0 { // the file itself
			return st.Mode&syscall.S_IFMT != syscall.S_IFREG || uint32(st.Mode)&modeOtherWrite != 0
		}
		dir = next
	}
	return true // p names "/", no file
}

// components splits a path into the names a walk reads, "" and "." dropped.
func components(p string) []string {
	var out []string
	for _, name := range strings.Split(p, "/") {
		if name != "" && name != "." {
			out = append(out, name)
		}
	}
	return out
}
