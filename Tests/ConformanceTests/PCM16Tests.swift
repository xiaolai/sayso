import Foundation
import PCM16
import XCTest

final class PCM16Tests: XCTestCase {
    func testPCM16() throws { try runFixtures("pcm16") { try checkPCM($0.object) } }

    private func checkPCM(_ c: [String: Any]) throws -> String? {
        switch c["op"] as? String {
        case "encode":
            guard let input = c["input"] as? [Any] else { return "bad case" }
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
            if let want = c["expectSamples"] as? [Int], ints(out) != want {
                problems.append("samples \(ints(out)), want \(want)")
            }
            if let want = c["expectHex"] as? String, hex(out) != want {
                problems.append("hex \(hex(out)), want \(want)")
            }
            return problems.isEmpty ? nil : problems.joined(separator: "; ")
        case "decode":
            let raw = try bytes(c["hex"])
            let from = c["sliceFrom"] as? Int ?? 0
            // A slice of a larger buffer, whose indices do not start at zero.
            let got = PCM.decode(raw[(raw.startIndex + from)...])
            let want = (c["expectSamples"] as? [Int] ?? []).map { Float($0) / 32768 }
            return got == want ? nil : "decoded \(got), want \(want)"
        case "wav":
            let got = hex(PCM.wav(try bytes(c["pcmHex"]), rate: c["rate"] as? Int ?? 0))
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
