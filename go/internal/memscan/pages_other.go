//go:build !unix

package memscan

// mapPages has no anonymous mapping to make here: New reports the platform unsupported.
func mapPages(int) ([]byte, error) { return nil, ErrUnsupported }

func unmapPages([]byte) {}
