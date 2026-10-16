import Foundation
import PCM16
import XCTest

final class PCM16Tests: XCTestCase {
    func testPCM16() throws { try runFixtures("pcm16") { try checkPCM($0.object) } }

    /// Every header `wav` writes, across rates and data lengths, holds each canonical field for that rate and length;
    /// `isValidWAV` accepts exactly the 24 kHz ones with a non-empty, even data chunk within the cap; and a rate no
    /// header can carry is refused.
    func testWAVHeaderAcrossRatesAndLengths() throws {
        for rate in [1, 2, 8000, 11025, 16000, 22050, PCM.sampleRate, 44100, 48000, 96000, PCM.maxRate] {
            for n in [0, 1, 2, 3, 4, 100, 4097] {
                let pcm = Data((0..<n).map { UInt8(truncatingIfNeeded: $0) })
                let wav = try PCM.wav(pcm, rate: rate)
                checkHeader(wav, rate: rate, pcm: pcm)
                let want = rate == PCM.sampleRate && n > 0 && n % 2 == 0
                XCTAssertEqual(PCM.isValidWAV(wav, maxData: n), want, "rate \(rate), \(n) bytes")
                if n > 0 { XCTAssertFalse(PCM.isValidWAV(wav, maxData: n - 1), "rate \(rate): over a cap of \(n - 1)") }
            }
        }
        for rate in [Int.min, -1, 0, PCM.maxRate + 1, Int.max] {
            XCTAssertThrowsError(try PCM.wav(Data([0, 0]), rate: rate), "rate \(rate)") {
                XCTAssertEqual($0 as? PCM.RangeError, PCM.RangeError())
            }
        }
    }

    /// Compares each field of `wav`'s header with the canonical value for `rate` and `pcm`, which must follow it.
    private func checkHeader(_ wav: Data, rate: Int, pcm: Data, line: UInt = #line) {
        let b = [UInt8](wav)
        guard b.count == PCM.headerSize + pcm.count else {
            return XCTFail("rate \(rate): \(b.count) bytes for \(pcm.count) of PCM", line: line)
        }
        func le16(_ at: Int) -> UInt32 { UInt32(b[at]) | UInt32(b[at + 1]) << 8 }
        func le32(_ at: Int) -> UInt32 { le16(at) | le16(at + 2) << 16 }
        func id(_ at: Int) -> String { String(decoding: b[at..<at + 4], as: UTF8.self) }
        let fields: [(String, AnyHashable, AnyHashable)] = [
            ("chunk id", id(0), "RIFF"), ("RIFF size", le32(4), UInt32(36 + pcm.count)), ("form", id(8), "WAVE"),
            ("fmt id", id(12), "fmt "), ("fmt size", le32(16), UInt32(16)), ("format", le16(20), UInt32(1)),
            ("channels", le16(22), UInt32(1)), ("rate", le32(24), UInt32(rate)),
            ("byte rate", le32(28), UInt32(2 * rate)), ("block align", le16(32), UInt32(2)),
            ("bits", le16(34), UInt32(16)), ("data id", id(36), "data"), ("data size", le32(40), UInt32(pcm.count)),
        ]
        for (name, got, want) in fields where got != want {
            XCTFail("rate \(rate), \(pcm.count) bytes: \(name) \(got), want \(want)", line: line)
        }
        XCTAssertEqual(Data(b[PCM.headerSize...]), pcm, "rate \(rate): the PCM after the header differs", line: line)
    }

    /// Per op, the case members it reads besides the head and `op`.
    private static let members: [String: Set<String>] = [
        "encode": ["input", "expectSamples", "expectHex"],
        "decode": ["hex", "sliceFrom", "expectSamples"],
        "wav": ["rate", "pcmHex", "expectHex", "expectError"],
        "validWAV": ["hex", "capBytes", "expect"],
        "plausible": ["pcm", "script", "expect"],
    ]

    private func checkPCM(_ c: [String: Any]) throws -> String? {
        if let op = c["op"] as? String, let known = Self.members[op] {
            let unknown =
                FakeOpenAI.unknownKeys("case", c, FakeOpenAI.caseHead.union(["op"]).union(known))
                + FakeOpenAI.unknownKeys("pcm", c["pcm"] as? [String: Any], ["samples", "value"])
            guard unknown.isEmpty else { return unknown.joined(separator: "; ") }
        }
        switch c["op"] as? String {
        case "encode":
            guard let input = c["input"] as? [Any] else { return "bad case" }
            // Required: without it the case would check the hex alone, or nothing.
            guard let wantSamples = c["expectSamples"] as? [Int] else { return "an encode case needs expectSamples" }
            let samples: [Float] = input.map { v in
                if let n = v as? NSNumber { return Float(n.doubleValue) }
                switch v as? String {
                case "NaN": return .nan
                case "Infinity": return .infinity
                case "-Infinity": return -.infinity
                default: return 0
                }
            }
            let out = PCM.encode(samples)
            var problems: [String] = []
            if ints(out) != wantSamples { problems.append("samples \(ints(out)), want \(wantSamples)") }
            if let want = c["expectHex"] as? String, hex(out) != want {
                problems.append("hex \(hex(out)), want \(want)")
            }
            return problems.isEmpty ? nil : problems.joined(separator: "; ")
        case "decode":
            let raw = try bytes(c["hex"])
            let from = c["sliceFrom"] as? Int ?? 0
            // A slice of a larger buffer, whose indices do not start at zero.
            let got = PCM.decode(raw[(raw.startIndex + from)...])
            guard let samples = c["expectSamples"] as? [Int] else { return "a decode case needs expectSamples" }
            let want = samples.map { Float($0) / 32768 }
            return got == want ? nil : "decoded \(got), want \(want)"
        case "wav":
            let pcm = try bytes(c["pcmHex"]), rate = c["rate"] as? Int ?? 0
            if c["expectError"] as? Bool == true {
                return (try? PCM.wav(pcm, rate: rate)) == nil ? nil : "wav succeeded, want an error"
            }
            let got = hex(try PCM.wav(pcm, rate: rate))
            return got == c["expectHex"] as? String ? nil : "wav \(got)"
        case "validWAV":
            guard let want = c["expect"] as? Bool else { return "bad case" }
            let got = PCM.isValidWAV(try bytes(c["hex"]), maxData: c["capBytes"] as? Int ?? 0)
            return got == want ? nil : "isValidWAV \(got), want \(want)"
        case "plausible":
            guard let want = c["expect"] as? Bool, let pcm = c["pcm"] as? [String: Any],
                let count = pcm["samples"] as? Int, let value = pcm["value"] as? Int
            else { return "bad case" }
            let s = UInt16(bitPattern: Int16(value))
            var data = Data()
            for _ in 0..<count { data.append(contentsOf: [UInt8(s & 0xFF), UInt8(s >> 8)]) }
            let got = PCM.isPlausible(data, script: c["script"] as? String ?? "")
            return got == want ? nil : "isPlausible \(got), want \(want)"
        default:
            return "unknown op \(c["op"] ?? "?")"
        }
    }

    private func ints(_ pcm: Data) -> [Int] {
        let b = [UInt8](pcm)
        return (0..<b.count / 2).map { Int(Int16(bitPattern: UInt16(b[2 * $0]) | UInt16(b[2 * $0 + 1]) << 8)) }
    }

    private func hex(_ d: Data) -> String { d.map { String(format: "%02x", $0) }.joined() }

    private func bytes(_ v: Any?) throws -> Data {
        guard let s = v as? String, s.count % 2 == 0 else { throw FixtureError("bad hex") }
        var out = Data()
        var i = s.startIndex
        while i < s.endIndex {
            let j = s.index(i, offsetBy: 2)
            guard let b = UInt8(s[i..<j], radix: 16) else { throw FixtureError("bad hex") }
            out.append(b)
            i = j
        }
        return out
    }
}
