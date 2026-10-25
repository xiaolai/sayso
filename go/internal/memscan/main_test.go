package memscan

import "testing"

// Only the last asyncpreemptoff entry counts, as the runtime reads GODEBUG.
func TestPreemptOffReadsTheEffectiveEntry(t *testing.T) {
	for godebug, want := range map[string]bool{
		"":                                           false,
		"asyncpreemptoff=1":                          true,
		"gctrace=1,asyncpreemptoff=1":                true,
		"asyncpreemptoff=1,asyncpreemptoff=0":        false,
		"asyncpreemptoff=0,asyncpreemptoff=1":        true,
		"asyncpreemptoff=0":                          false,
		"xasyncpreemptoff=1":                         false,
		"asyncpreemptoff=1,gctrace=1,madvdontneed=1": true,
	} {
		if got := preemptOff(godebug); got != want {
			t.Errorf("preemptOff(%q) = %v, want %v", godebug, got, want)
		}
	}
}
