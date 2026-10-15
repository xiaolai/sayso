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
}
