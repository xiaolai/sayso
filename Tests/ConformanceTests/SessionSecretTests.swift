import Foundation
import XCTest

@testable import RealtimeConnection

/// The scrubber and error redaction (spec/credentials.md, hygiene), beyond what the fixtures reach.
final class SessionSecretTests: XCTestCase {
    func testCombiningMarkAfterSecretDoesNotHideIt() {
        Secret.register("opaque-value-combining")
        XCTAssertFalse(Secret.scrub("a opaque-value-combining\u{0301} b").contains("opaque-value"))
    }

    func testCanonicallyEquivalentSecretsAreBothRegistered() {
        Secret.register("caf\u{E9}-precomposed-secret")
        Secret.register("cafe\u{0301}-precomposed-secret")
        XCTAssertFalse(Secret.scrub("x cafe\u{0301}-precomposed-secret y").contains("precomposed"))
    }

    func testRegisteredInsidePatternRemovesTheWholeValue() {
        Secret.register("ek_demo")
        XCTAssertEqual(Secret.scrub("x ek_demo_sensitive y"), "x <redacted> y")
    }

    func testOverlappingOccurrences() {
        Secret.register("ovlap-ovlap")
        XCTAssertEqual(Secret.scrub("ovlap-ovlap-ovlap"), "<redacted>")
    }

    func testRepeatedPrefixIsLinear() throws {
        // A sanitizer slows this single-threaded scan some twentyfold, to about the bound, so under one the clock
        // measures the sanitizer. The unsanitized runs keep the bound, which the old scan missed by orders of
        // magnitude.
        if MemoryScan.allocatorReplaced { throw XCTSkip("a sanitizer's slowdown, not the scan's, would be timed") }
        let s = String(repeating: "eyJ", count: 60_000)
        let start = Date()
        XCTAssertEqual(Secret.scrub(s), s)
        XCTAssertLessThan(Date().timeIntervalSince(start), 30)  // generous for a loaded machine; the old scan took about 5·10⁹ steps here
    }

    func testPatternsMatchGo() {
        XCTAssertEqual(Secret.scrub("Bearer eyJa.eyJb.c and ek_live_1"), "Bearer <redacted> and <redacted>")
        XCTAssertEqual(Secret.scrub("eyJa.b"), "eyJa.b")
        XCTAssertEqual(Secret.scrub("ek_"), "ek_")
    }

    func testErrorFieldsAreScrubbedWhenBuiltAndWhenPrinted() {
        let e = RealtimeError(
            .server, type: "ek_canary_type", code: "ek_canary_code", param: "ek_canary_param",
            message: "ek_canary_message", requestID: "ek_canary_request", route: "ek_canary_route")
        for field in [e.type, e.code, e.param, e.message, e.requestID, e.route] {
            XCTAssertFalse(field.contains("canary"), field)
        }
        var mutated = e
        mutated.message = "ek_canary_after"
        mutated.cause = "ek_canary_cause"
        XCTAssertFalse(mutated.description.contains("canary"), mutated.description)
    }
}
