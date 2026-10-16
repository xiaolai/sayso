package codexapp

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// waitExit returns when the child has exited, leaving it a zombie: waitid(P_PID, pid, WEXITED|WNOWAIT).
// A zombie keeps its process ID, and so its group ID, ours: Close can signal the group without reaching a group that
// reused the ID, and reaps the child last.
func waitExit(cmd *exec.Cmd) {
	const pPID, wExited, wNoWait = 1, 0x4, 0x20
	var info [128]byte // siginfo_t, 104 bytes on darwin; unread
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(cmd.Process.Pid),
			uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		if e != syscall.EINTR {
			return
		}
	}
}

// groupAlive reports a live member of the group. While the leader is an unreaped zombie, kill(-pgid, 0) fails with
// EPERM once no live member remains (measured on macOS), and succeeds while one does.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }
