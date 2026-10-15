// Package pcm16 encodes, decodes and checks realtime audio: 16-bit signed little-endian mono PCM (spec/pcm16.md).
package pcm16

import (
	"encoding/binary"
	"math"
	"unicode"
)

// SampleRate is the realtime audio rate, in and out.
const SampleRate = 24000

// HeaderSize is the size of the canonical WAV header.
const HeaderSize = 44

// minPeak is the plausibility threshold, about -40 dBFS.
const minPeak = 328

// Encode converts floats to PCM: each value is clamped to [-1, 1], a non-finite value becomes 0, and the sample is
// trunc(v × 32767).
func Encode(samples []float32) []byte {
	out := make([]byte, 2*len(samples))
	for i, v := range samples {
		f := float64(v)
		switch {
		case math.IsNaN(f) || math.IsInf(f, 0):
			f = 0
		case f > 1:
			f = 1
		case f < -1:
			f = -1
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(f*32767)))
	}
	return out
}

// Decode converts PCM to floats, each sample / 32768. An odd trailing byte is ignored. It indexes from pcm's own
// start, so a slice of a larger buffer decodes like a copy.
func Decode(pcm []byte) []float32 {
	out := make([]float32, len(pcm)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return out
}

// WAV returns pcm behind a canonical 44-byte mono 16-bit header for rate.
func WAV(pcm []byte, rate int) []byte {
	out := make([]byte, HeaderSize, HeaderSize+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:], 1) // mono
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	return append(out, pcm...)
}

// ValidWAV reports whether wav is exactly the canonical header at 24 kHz with consistent sizes, followed by a
// non-empty, even-length data chunk of at most maxData bytes.
func ValidWAV(wav []byte, maxData int) bool {
	n := len(wav) - HeaderSize
	if n <= 0 || n%2 != 0 || n > maxData {
		return false
	}
	want := WAV(nil, SampleRate)
	binary.LittleEndian.PutUint32(want[4:], uint32(36+n))
	binary.LittleEndian.PutUint32(want[40:], uint32(n))
	return string(wav[:HeaderSize]) == string(want)
}

// Plausible reports whether pcm can be a reading of script: at least one second per 25 letters of the script, and
// some sample at least minPeak in magnitude.
func Plausible(pcm []byte, script string) bool {
	letters := 0
	for _, r := range script {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	// bytes / (2 × SampleRate) ≥ letters / 25, in integers.
	if len(pcm)*25 < letters*2*SampleRate {
		return false
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if s >= minPeak || s <= -minPeak {
			return true
		}
	}
	return false
}
