//go:build darwin || linux

package codexapp

import (
	"os/exec"
	"syscall"
	"unsafe"
)

const pPID, wNoHang, wExited = 1, 0x1, 0x4

// waitExit returns when the child has exited, leaving it a zombie: waitid(P_PID, pid, WEXITED|WNOWAIT). A zombie
// keeps its process ID, and so its group ID, ours: Close can signal the group without reaching a group that reused the
// ID, and reaps the child last. It reports whether that holds: false when waitid failed, as it does with ECHILD once
// the kernel reaped the child itself because the host ignores SIGCHLD, when the ID may name another process.
func waitExit(cmd *exec.Cmd) bool { return waitid(cmd.Process.Pid, wExited|wNoWait) }

// held reports, without waiting, whether pid is still our unreaped child, alive or a zombie.
func held(pid int) bool { return waitid(pid, wExited|wNoWait|wNoHang) }

func waitid(pid, options int) bool {
	var info [128]byte // siginfo_t: 104 bytes on darwin, 128 on Linux; unread
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])),
			uintptr(options), 0, 0)
		if e != syscall.EINTR {
			return e == 0
		}
	}
}
