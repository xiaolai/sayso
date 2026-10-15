# PCM16 (`PCM`)

Component `pcm16`. Product `PCM16` (Swift), package `pcm16` (Go). Fixtures: `fixtures/pcm16.json`.

Realtime audio is 16-bit signed little-endian mono PCM at 24 kHz.

- **Encode** (`PCM-01`): each float is clamped to [-1, 1], a non-finite value becomes 0, and the result is
  `trunc(v × 32767)` as an `Int16`.
- **Decode** (`PCM-02`): each pair of bytes is a little-endian `Int16`, divided by 32768. An odd trailing byte is
  ignored. Decoding indexes from the buffer's own start, so a slice of a larger buffer decodes like a copy.
- **WAV** (`PCM-03`): a 44-byte canonical header (`RIFF`, size, `WAVEfmt `, 16, PCM = 1, mono, rate, rate × 2, block
  2, 16 bits, `data`, size) for a given sample rate, followed by the PCM. A rate outside 1…2,147,483,647 (rate × 2
  must fit 32 bits) or PCM longer than 4,294,967,259 bytes (36 + size must fit 32 bits) is an error, never a header
  with wrapped sizes.
- **WAV validation** (`PCM-04`): true only for exactly that header at 24 kHz, sizes consistent with the data, an even
  data length, more than 44 bytes, and data no longer than the caller's cap or than a header can describe.
- **Plausibility** (`PCM-05`): a take is plausible when its duration (bytes / 48,000 s) is at least one second per 25
  letters of the script (Unicode letters) and some sample's magnitude is at least 328 (about -40 dBFS).
