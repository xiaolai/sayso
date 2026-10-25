//go:build darwin || linux

package codexapp

import (
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

const pPID, wNoHang, wExited = 1, 0x1, 0x4

// waitExit returns when the child has exited, leaving it a zombie: waitid(P_PID, pid, WEXITED|WNOWAIT). A zombie
// keeps its process ID, and so its group ID, ours: Close can signal the group without reaching a group that reused the
// ID, and reaps the child last. It reports whether that holds: false when waitid fails with ECHILD, as it does once
// the kernel reaped the child itself because the host ignores SIGCHLD, when the ID may name another process. Any other
// failure says nothing about the child, so the wait is tried again (Start refuses a host where it cannot work).
func waitExit(cmd *exec.Cmd) bool {
	for {
		switch e := waitid(cmd.Process.Pid, wExited|wNoWait); e {
		case 0:
			return true
		case syscall.ECHILD:
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// watchable reports why waitExit and held cannot work for the just-started child pid: nil, or the waitid error. ECHILD
// is no such reason: a child the kernel already reaped is the loss waitExit reports.
func watchable(pid int) error {
	if e := waitid(pid, wExited|wNoWait|wNoHang); e != 0 && e != syscall.ECHILD {
		return e
	}
	return nil
}

// held reports, without waiting, whether pid is still our unreaped child, alive or a zombie.
func held(pid int) bool { return waitid(pid, wExited|wNoWait|wNoHang) == 0 }

// waitid returns 0 or the error waitid failed with, EINTR retried.
func waitid(pid, options int) syscall.Errno {
	var info [128]byte // siginfo_t: 104 bytes on darwin, 128 on Linux; unread
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])),
			uintptr(options), 0, 0)
		if e != syscall.EINTR {
			return e
		}
	}
}
