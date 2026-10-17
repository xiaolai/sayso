//go:build !linux

package memscan

// Explain is Linux-only: elsewhere it says nothing.
func (s *Scan) Explain() string { return "" }
