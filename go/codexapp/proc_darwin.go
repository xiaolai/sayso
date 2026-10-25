package codexapp

import "syscall"

// wNoWait is darwin's WNOWAIT.
const wNoWait = 0x20

// groupAlive reports a live member of the group. While the leader is an unreaped zombie, kill(-pgid, 0) fails with
// EPERM once no live member remains (measured on macOS), and succeeds while one does. EPERM also comes when live
// members remain but none of them may be signalled by us; false is then still the right answer for Close, which could
// not signal them either.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }
