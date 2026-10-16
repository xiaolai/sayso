//go:build !linux && !(darwin && cgo)

package memscan

import "unsafe"

func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

func regions(func(lo, hi uintptr)) error { return ErrUnsupported }

func read(uintptr, []byte) int { return 0 }
