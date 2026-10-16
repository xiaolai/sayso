import XCTest

@testable import RealtimeConnection

/// Redaction and scrubbing beyond the fixtures (spec/credentials.md, hygiene).
final class SecretHygieneTests: XCTestCase {
    func testErrorFieldsAreScrubbedWhenSet() {
        var e = RealtimeError(.server, message: "ek_canary_init")
        e.message = "ek_canary_after"
        e.type = "ek_canary_type"
        e.requestID = "eyJa.b.c"
        var dumped = ""
        dump(e, to: &dumped)
        for text in [e.description, dumped, String(describing: Mirror(reflecting: e).children.map { $0.value })] {
            XCTAssertFalse(text.contains("canary"), "a field set after construction leaked")
            XCTAssertFalse(text.contains("eyJa.b.c"))
        }
        XCTAssertEqual(e.message, Secret.redacted)
    }

    func testSecretsCompareByBytes() {
        let composed = "tok-\u{E9}", decomposed = "tok-e\u{301}"
        XCTAssertNotEqual(Token(bearer: composed, expires: nil), Token(bearer: decomposed, expires: nil))
        XCTAssertEqual(Token(bearer: composed, expires: nil), Token(bearer: composed, expires: nil))
        let at = Date(timeIntervalSince1970: 1)
        XCTAssertNotEqual(ClientSecret(bearer: composed, expiresAt: at), ClientSecret(bearer: decomposed, expiresAt: at))
    }

    func testJWTInsideAnEKMatchIsRemovedWhole() {
        XCTAssertEqual(Secret.scrub("x ek_eyJa.b.c y"), "x \(Secret.redacted) y")
    }

    func testRepetitiveSecretIsLinear() {
        let secret = String(repeating: "a", count: 32_768) + "b"
        Secret.register(secret)
        let text = String(repeating: "a", count: 65_536)
        let start = Date()
        XCTAssertEqual(Secret.scrub(text), text)
        XCTAssertEqual(Secret.scrub(text + "b"), String(repeating: "a", count: 32_768) + Secret.redacted)
        // A naive scan is about 10^9 comparisons; the bound is generous for a loaded machine.
        XCTAssertLessThan(Date().timeIntervalSince(start), 30)
    }

    /// The rolling match finds exactly the windows equal to a registered secret, overlapping ones included, against a
    /// direct comparison, over a two-letter alphabet where near misses are everywhere; as in Go.
    func testRollingMatchIsExact() {
        var rng = SplitMix64(seed: 1)
        func word(_ n: Int) -> [UInt8] { (0..<n).map { _ in Bool.random(using: &rng) ? 0x61 : 0x62 } }
        for _ in 0..<2000 {
            let registry = Secret.Registry()
            let text = word(Int.random(in: 0..<64, using: &rng))
            let n = Int.random(in: 1...8, using: &rng)
            var secrets: Set<[UInt8]> = []
            for _ in 0..<Int.random(in: 1...4, using: &rng) {
                var secret = word(n)
                if Bool.random(using: &rng), text.count >= n {  // often one that occurs
                    let at = Int.random(in: 0...(text.count - n), using: &rng)
                    secret = Array(text[at..<at + n])
                }
                secrets.insert(secret)
                registry.add(String(decoding: secret, as: UTF8.self))
            }
            let want = text.count >= n ? (0...(text.count - n)).filter { secrets.contains(Array(text[$0..<$0 + n])) } : []
            let got = registry.spans(in: text).map(\.lowerBound).sorted()
            XCTAssertEqual(got, want, "text \(text), secrets \(secrets)")
            XCTAssert(registry.spans(in: text).allSatisfy { $0.count == n })
        }
    }

    /// Thue-Morse strings collide under any odd base modulo 2^64; modulo 2^61-1 the complementary pair stays apart.
    func testThueMorseStringsDoNotCollide() {
        func thueMorse(_ n: Int, flip: Bool) -> [UInt8] {
            (0..<n).map { i in (i.nonzeroBitCount % 2 == 1) != flip ? 0x62 : 0x61 }
        }
        let x = thueMorse(2048, flip: false), y = thueMorse(2048, flip: true)
        for _ in 0..<20 {
            let registry = Secret.Registry()
            XCTAssertNotEqual(registry.pair(x), registry.pair(y), "a Thue-Morse pair collides")
        }
    }

    /// Registering a secret adds no copy of it to memory: the registry keeps its length and hashes only.
    func testRegisterKeepsNoCopy() throws {
        if MemoryScan.allocatorReplaced { throw XCTSkip("a sanitizer replaced the allocator the scan watches") }
        let scan = MemoryScan()
        let n = scan.needle()
        let secret = String(decoding: scan.bytes(n), as: UTF8.self)
        let before = scan.occurrences()[0]
        XCTAssertGreaterThan(before, 0, "the scan missed the caller's own copy")
        Secret.register(secret)
        XCTAssertEqual(scan.occurrences()[0], before, "the registry kept a copy of the secret")
        XCTAssertEqual(Secret.scrub("x " + secret + " y"), "x \(Secret.redacted) y")
    }
}

/// A seeded generator, so a failing case can be replayed.
struct SplitMix64: RandomNumberGenerator {
    private var state: UInt64
    init(seed: UInt64) { state = seed }
    mutating func next() -> UInt64 {
        state &+= 0x9E37_79B9_7F4A_7C15
        var z = state
        z = (z ^ (z >> 30)) &* 0xBF58_476D_1CE4_E5B9
        z = (z ^ (z >> 27)) &* 0x94D0_49BB_1331_11EB
        return z ^ (z >> 31)
    }
}
