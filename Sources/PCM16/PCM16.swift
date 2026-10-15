import Foundation

/// Realtime audio: 16-bit signed little-endian mono PCM (spec/pcm16.md).
public enum PCM {
    /// The realtime audio rate, in and out.
    public static let sampleRate = 24_000

    /// The size of the canonical WAV header.
    public static let headerSize = 44

    /// The plausibility threshold, about -40 dBFS.
    static let minPeak = 328

    /// Converts floats to PCM: each value is clamped to [-1, 1], a non-finite value becomes 0, and the sample is
    /// trunc(v × 32767).
    public static func encode(_ samples: [Float]) -> Data {
        var out = Data(count: 2 * samples.count)
        out.withUnsafeMutableBytes { (raw: UnsafeMutableRawBufferPointer) in
            for (i, v) in samples.enumerated() {
                var f = Double(v)
                if !f.isFinite {
                    f = 0
                } else if f > 1 {
                    f = 1
                } else if f < -1 {
                    f = -1
                }
                let s = UInt16(bitPattern: Int16(f * 32767))
                raw[2 * i] = UInt8(s & 0xFF)
                raw[2 * i + 1] = UInt8(s >> 8)
            }
        }
        return out
    }

    /// Converts PCM to floats, each sample / 32768. An odd trailing byte is ignored. It indexes from the buffer's own
    /// start, so a slice of a larger buffer decodes like a copy.
    public static func decode(_ pcm: Data) -> [Float] {
        let bytes = [UInt8](pcm)
        return (0..<bytes.count / 2).map { i in
            Float(sample(bytes, at: 2 * i)) / 32768
        }
    }

    /// `pcm` behind a canonical 44-byte mono 16-bit header for `rate`.
    public static func wav(_ pcm: Data, rate: Int) -> Data {
        var out = Data(capacity: headerSize + pcm.count)
        out.append(contentsOf: Array("RIFF".utf8))
        append32(&out, UInt32(truncatingIfNeeded: 36 + pcm.count))
        out.append(contentsOf: Array("WAVEfmt ".utf8))
        append32(&out, 16)
        append16(&out, 1)  // PCM
        append16(&out, 1)  // mono
        append32(&out, UInt32(truncatingIfNeeded: rate))
        append32(&out, UInt32(truncatingIfNeeded: rate * 2))
        append16(&out, 2)
        append16(&out, 16)
        out.append(contentsOf: Array("data".utf8))
        append32(&out, UInt32(truncatingIfNeeded: pcm.count))
        out.append(pcm)
        return out
    }

    /// Whether `wav` is exactly the canonical header at 24 kHz with consistent sizes, followed by a non-empty,
    /// even-length data chunk of at most `maxData` bytes.
    public static func isValidWAV(_ wav: Data, maxData: Int) -> Bool {
        let n = wav.count - headerSize
        guard n > 0, n % 2 == 0, n <= maxData else { return false }
        let bytes = [UInt8](wav)
        var want = [UInt8](self.wav(Data(), rate: sampleRate))
        put32(&want, at: 4, UInt32(36 + n))
        put32(&want, at: 40, UInt32(n))
        return Array(bytes[0..<headerSize]) == want
    }

    /// Whether `pcm` can be a reading of `script`: at least one second per 25 letters of the script, and some sample
    /// at least `minPeak` in magnitude.
    public static func isPlausible(_ pcm: Data, script: String) -> Bool {
        let letters = script.unicodeScalars.filter(isLetter).count
        // bytes / (2 × sampleRate) ≥ letters / 25, in integers.
        if pcm.count * 25 < letters * 2 * sampleRate { return false }
        let bytes = [UInt8](pcm)
        var i = 0
        while i + 1 < bytes.count {
            let s = Int(sample(bytes, at: i))
            if s >= minPeak || s <= -minPeak { return true }
            i += 2
        }
        return false
    }

    /// Go's unicode.IsLetter: general category L.
    private static func isLetter(_ r: Unicode.Scalar) -> Bool {
        switch r.properties.generalCategory {
        case .uppercaseLetter, .lowercaseLetter, .titlecaseLetter, .modifierLetter, .otherLetter: return true
        default: return false
        }
    }

    private static func sample(_ b: [UInt8], at i: Int) -> Int16 {
        Int16(bitPattern: UInt16(b[i]) | UInt16(b[i + 1]) << 8)
    }

    private static func append16(_ d: inout Data, _ v: UInt16) {
        d.append(contentsOf: [UInt8(v & 0xFF), UInt8(v >> 8)])
    }

    private static func append32(_ d: inout Data, _ v: UInt32) {
        d.append(contentsOf: [UInt8(v & 0xFF), UInt8(v >> 8 & 0xFF), UInt8(v >> 16 & 0xFF), UInt8(v >> 24)])
    }

    private static func put32(_ b: inout [UInt8], at i: Int, _ v: UInt32) {
        b[i] = UInt8(v & 0xFF)
        b[i + 1] = UInt8(v >> 8 & 0xFF)
        b[i + 2] = UInt8(v >> 16 & 0xFF)
        b[i + 3] = UInt8(v >> 24)
    }
}
