//go:build unix

package memscan

import "syscall"

// mapPages maps n bytes of private anonymous memory, apart from the Go heap.
func mapPages(n int) ([]byte, error) {
	return syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
}

func unmapPages(b []byte) { _ = syscall.Munmap(b) }
