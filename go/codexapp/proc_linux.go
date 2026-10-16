package codexapp

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

// waitExit returns when the child has exited, leaving it a zombie: waitid(P_PID, pid, WEXITED|WNOWAIT).
// A zombie keeps its process ID, and so its group ID, ours: Close can signal the group without reaching a group that
// reused the ID, and reaps the child last.
func waitExit(cmd *exec.Cmd) {
	const pPID, wExited, wNoWait = 1, 0x4, 0x1000000
	var info [128]byte // siginfo_t, 128 bytes on Linux; unread
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(cmd.Process.Pid),
			uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		if e != syscall.EINTR {
			return
		}
	}
}

// groupAlive reports a live member of the group. Linux counts a zombie leader as a member, so kill(-pgid, 0) cannot
// tell a group holding only our unreaped child from one with a live member: /proc decides, a member being live
// unless it is a zombie (Z) or dead (X). Without /proc the group counts as alive, which costs Close the grace but
// never signals another group, since the unreaped child keeps the ID ours.
func groupAlive(pgid int) bool {
	if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	d, err := os.Open("/proc")
	if err != nil {
		return true
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return true
	}
	want := []byte(strconv.Itoa(pgid))
	for _, name := range names {
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		stat, err := os.ReadFile("/proc/" + name + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		// pid (comm) state ppid pgrp ...: comm may hold spaces and parentheses, so the fields start after the last ')'.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := bytes.Fields(stat[i+1:])
		if len(f) >= 3 && bytes.Equal(f[2], want) && !bytes.Equal(f[0], []byte("Z")) && !bytes.Equal(f[0], []byte("X")) {
			return true
		}
	}
	return false
}
