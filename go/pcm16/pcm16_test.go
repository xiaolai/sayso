package pcm16

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
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

// Every header WAV writes, across rates and data lengths, holds each canonical field for that rate and length; ValidWAV
// accepts exactly the 24 kHz ones with a non-empty, even data chunk within the cap; and a rate no header can carry is
// refused.
func TestWAVHeaderAcrossRatesAndLengths(t *testing.T) {
	for _, rate := range []int{1, 2, 8000, 11025, 16000, 22050, SampleRate, 44100, 48000, 96000, MaxRate} {
		for _, n := range []int{0, 1, 2, 3, 4, 100, 4097} {
			pcm := make([]byte, n)
			for i := range pcm {
				pcm[i] = byte(i)
			}
			wav, err := WAV(pcm, rate)
			if err != nil {
				t.Fatalf("rate %d, %d bytes: %v", rate, n, err)
			}
			checkHeader(t, wav, rate, pcm)
			want := rate == SampleRate && n > 0 && n%2 == 0
			if got := ValidWAV(wav, n); got != want {
				t.Errorf("rate %d, %d bytes: ValidWAV %v, want %v", rate, n, got, want)
			}
			if n > 0 && ValidWAV(wav, n-1) {
				t.Errorf("rate %d: %d bytes accepted under a cap of %d", rate, n, n-1)
			}
		}
	}
	refused := []int{math.MinInt, -1, 0}
	if over := int64(MaxRate) + 1; over <= math.MaxInt { // a rate past MaxRate is not an int on a 32-bit platform
		refused = append(refused, int(over), math.MaxInt)
	}
	for _, rate := range refused {
		if _, err := WAV([]byte{0, 0}, rate); !errors.Is(err, ErrWAVRange) {
			t.Errorf("rate %d: %v, want ErrWAVRange", rate, err)
		}
	}
}

// checkHeader compares each field of wav's header with the canonical value for rate and pcm, which must follow it.
func checkHeader(t *testing.T, wav []byte, rate int, pcm []byte) {
	t.Helper()
	if len(wav) != HeaderSize+len(pcm) {
		t.Fatalf("rate %d: %d bytes for %d of PCM", rate, len(wav), len(pcm))
	}
	le16 := func(at int) uint32 { return uint32(binary.LittleEndian.Uint16(wav[at:])) }
	le32 := func(at int) uint32 { return binary.LittleEndian.Uint32(wav[at:]) }
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"chunk id", string(wav[0:4]), "RIFF"},
		{"RIFF size", le32(4), uint32(36 + len(pcm))},
		{"form", string(wav[8:12]), "WAVE"},
		{"fmt id", string(wav[12:16]), "fmt "},
		{"fmt size", le32(16), uint32(16)},
		{"format", le16(20), uint32(1)},
		{"channels", le16(22), uint32(1)},
		{"rate", le32(24), uint32(rate)},
		{"byte rate", le32(28), uint32(2 * uint64(rate))},
		{"block align", le16(32), uint32(2)},
		{"bits", le16(34), uint32(16)},
		{"data id", string(wav[36:40]), "data"},
		{"data size", le32(40), uint32(len(pcm))},
	} {
		if f.got != f.want {
			t.Errorf("rate %d, %d bytes: %s %v, want %v", rate, len(pcm), f.name, f.got, f.want)
		}
	}
	if !bytes.Equal(wav[HeaderSize:], pcm) {
		t.Errorf("rate %d: the PCM after the header differs", rate)
	}
}
