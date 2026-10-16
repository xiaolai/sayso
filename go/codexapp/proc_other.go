//go:build !darwin && !linux

package codexapp

import (
	"errors"
	"os/exec"
	"syscall"
)

// reservesGroup is false on the systems without a waitid this package calls (the BSDs other than macOS): the child is
// reaped as soon as it exits, and Close signals the group only while kill(-pgid, 0) shows it alive, which leaves the
// short window in which a reaped ID could be reused. macOS and Linux reserve the group.
const reservesGroup = false

func waitExit(cmd *exec.Cmd) { _ = cmd.Wait() }

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
