package pcm16

import (
	"strings"
	"testing"
)

// The duration check runs in int64: on a 32-bit platform 44,740 letters × 48,000 overflowed int, and one loud
// sample then passed for a reading of the whole script.
func TestPlausibleDurationDoesNotOverflow(t *testing.T) {
	pcm := []byte{0x48, 0x01} // one sample of 328, above minPeak
	if Plausible(pcm, strings.Repeat("a", 44740)) {
		t.Fatal("one sample was plausible for 44,740 letters")
	}
}

// Validation compares the header without building a payload-sized buffer.
func TestValidWAVAllocatesNoPayload(t *testing.T) {
	wav, err := WAV(make([]byte, 1<<20), SampleRate)
	if err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(10, func() { ValidWAV(wav, 1<<21) }); allocs != 0 {
		t.Fatalf("ValidWAV allocated %v times", allocs)
	}
}
