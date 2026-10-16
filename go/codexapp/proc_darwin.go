package codexapp

import "syscall"

// wNoWait is darwin's WNOWAIT.
const wNoWait = 0x20

// groupAlive reports a live member of the group. While the leader is an unreaped zombie, kill(-pgid, 0) fails with
// EPERM once no live member remains (measured on macOS), and succeeds while one does.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }
