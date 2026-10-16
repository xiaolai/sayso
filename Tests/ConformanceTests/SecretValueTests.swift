import Foundation
import XCTest

@testable import RealtimeConnection

/// The secret types beyond the fixtures: how an expiry is converted and normalised, and that no reflection or
/// encoding path shows a bearer.
final class SecretValueTests: XCTestCase {
    // MARK: Expiry conversion

    func testUnixSecondsRoundsDownAndSaturates() {
        let cases: [(TimeInterval, Int64)] = [
            (0, 0), (1.5, 1), (-0.5, -1), (-1.5, -2), (-1, -1), (1_800_000_000.999, 1_800_000_000),
            (0x1p62, 1 << 62), (0x1p63, .max), (1e19, .max), (-0x1p63, .min), (-1e19, .min),
            (.infinity, .max), (-.infinity, .min), (.nan, .min),
        ]
        for (t, want) in cases {
            XCTAssertEqual(ClientSecret.unixSeconds(Date(timeIntervalSince1970: t)), want, "\(t)")
        }
    }

    /// A non-finite expiry is made definite in both initialisers' sense, so the secret equals itself and its two
    /// expiries agree: NaN and minus infinity are long past, plus infinity the distant future.
    func testNonFiniteExpiriesAreDefinite() {
        let cases: [(TimeInterval, Date)] = [(.nan, .distantPast), (-.infinity, .distantPast), (.infinity, .distantFuture)]
        for (t, want) in cases {
            let s = ClientSecret(bearer: "ek_nonfinite", expiresAt: Date(timeIntervalSince1970: t))
            XCTAssertEqual(s.expiresAt, want, "\(t)")
            XCTAssertEqual(s.expiresAtUnix, ClientSecret.unixSeconds(want), "\(t)")
            XCTAssertEqual(s, s, "\(t): a secret unequal to itself")
            let token = Token(bearer: "opaque-nonfinite", expires: Date(timeIntervalSince1970: t))
            XCTAssertEqual(token.expires, want, "\(t)")
            XCTAssertEqual(token, token, "\(t): a token unequal to itself")
        }
        XCTAssertNil(Token(bearer: "opaque-unknown", expires: nil).expires)
    }

    /// Both initialisers agree on a whole-second expiry, and the integer one keeps a value a Date cannot hold.
    func testInitialisersAgree() {
        for unix: Int64 in [0, -1, 1_800_000_600, 1 << 53, -(1 << 53)] {
            let a = ClientSecret(bearer: "ek_agree", expiresAtUnix: unix)
            let b = ClientSecret(bearer: "ek_agree", expiresAt: Date(timeIntervalSince1970: TimeInterval(unix)))
            XCTAssertEqual(a, b, "\(unix)")
        }
        for unix: Int64 in [(1 << 53) + 1, .max, .min] {
            XCTAssertEqual(ClientSecret(bearer: "ek_exact", expiresAtUnix: unix).expiresAtUnix, unix)
        }
    }

    // MARK: Reflection and encoding

    private struct Holder: Encodable {
        let token: Token
        let secret: ClientSecret
        let list: [ClientSecret]
        let map: [String: Token]
    }

    private func holder() -> Holder {
        let token = Token(bearer: "opaque-canary-reflect", expires: Date(), plan: "plus", source: "test")
        let secret = ClientSecret(bearer: "ek_canary_reflect", expiresAtUnix: 1_800_000_600)
        return Holder(token: token, secret: secret, list: [secret], map: ["k": token])
    }

    private func assertNoBearer(_ text: String, _ what: String, file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertFalse(
            text.contains("opaque-canary-reflect") || text.contains("ek_canary_reflect"), "\(what): \(text)",
            file: file, line: line)
    }

    func testReflectionShowsNoBearer() {
        let h = holder()
        for (value, what) in [(h.token as Any, "token"), (h.secret, "secret"), (h, "holder"), ([h], "array")] {
            var dumped = ""
            dump(value, to: &dumped)
            assertNoBearer(dumped, "dump of \(what)")
            assertNoBearer(String(reflecting: value), "reflecting \(what)")
            assertNoBearer("\(value)", "describing \(what)")
            var children = ""
            func walk(_ v: Any, depth: Int) {
                guard depth < 8 else { return }
                for c in Mirror(reflecting: v).children {
                    children += "\(c.label ?? "") \(c.value)\n"
                    walk(c.value, depth: depth + 1)
                }
            }
            walk(value, depth: 0)
            assertNoBearer(children, "mirror of \(what)")
        }
    }

    func testEncodingShowsNoBearer() throws {
        let h = holder()
        let json = try JSONEncoder().encode(h)
        assertNoBearer(String(decoding: json, as: UTF8.self), "JSON")
        let plist = try PropertyListEncoder().encode(h)
        assertNoBearer(String(decoding: plist, as: UTF8.self), "binary property list")
        let xml = PropertyListEncoder()
        xml.outputFormat = .xml
        assertNoBearer(String(decoding: try xml.encode(h), as: UTF8.self), "XML property list")
        XCTAssertTrue(String(decoding: json, as: UTF8.self).contains(Secret.redacted), "the redacted marker is encoded")
    }
}
