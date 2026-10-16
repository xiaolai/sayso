import Foundation
import XCTest

@testable import RealtimeConnection

/// The secret types beyond the fixtures: how an expiry is converted and normalised.
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
}
