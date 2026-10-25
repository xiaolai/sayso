//go:build !linux && !(darwin && cgo)

package memscan

func regions(func(lo, hi uintptr)) error { return ErrUnsupported }

func read(uintptr, []byte) int { return 0 }
