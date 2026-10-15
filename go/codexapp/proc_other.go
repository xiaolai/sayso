//go:build !darwin

package codexapp

import (
	"errors"
	"os/exec"
	"syscall"
)

// reservesGroup is false here: Linux counts a zombie leader as a live member of its group, so the darwin scheme would
// make every close wait the whole grace. The child is reaped as soon as it exits, and Close signals the group only
// while kill(-pgid, 0) shows it alive, which leaves the short window in which a reaped ID could be reused.
const reservesGroup = false

func waitExit(cmd *exec.Cmd) { _ = cmd.Wait() }

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
