//go:build !darwin && !linux

package codexapp

// codexapp builds on macOS and Linux only. Elsewhere it has no way, wired here, to keep an exited child unreaped, so
// the child's process-group ID could name another group while Close signals it (spec/credentials.md). Rather than
// risk signalling a stranger's processes, the package does not build: the undefined name below is the error.
var _ = codexappBuildsOnMacOSAndLinuxOnly

// Declared so the name above is the only error a build here reports.
func waitExit(any) bool   { return false }
func held(int) bool       { return false }
func groupAlive(int) bool { return true }
