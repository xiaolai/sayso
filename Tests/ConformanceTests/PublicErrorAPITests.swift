#if os(macOS)
import CodexAuth
import RealtimeConnection
import XCTest

/// What an app reads and builds from sayso's errors without `@testable`: this file imports the products plainly, so it
/// compiles only while these stay public.
final class PublicErrorAPITests: XCTestCase {
    /// An app's tests can build the error a cancelled fetch ends with: the state it reached, the class cancelled.
    func testACancelledErrorKeepsItsStateAndIsClassedCancelled() {
        for state in CodexAuthState.allCases {
            let e = CodexAuthError.cancelled(state: state)
            XCTAssertEqual(e.state, state)
            XCTAssertEqual(e.errorClass, .cancelled)
            XCTAssertEqual(ErrorClass.of(e), .cancelled)
            XCTAssertEqual(CodexAuthError.state(of: e), state)
        }
    }

    /// An event that cannot be decoded names its type in a field of its own, never only in the message.
    func testAnUndecodableEventNamesItsType() throws {
        for (raw, type) in [
            (#"{"type":"response.created","response":{"id":1}}"#, "response.created"),
            (#"{"type":"rate_limits.updated","rate_limits":7}"#, "rate_limits.updated"),
            ("[", ""),
        ] {
            XCTAssertThrowsError(try RealtimeEvent.decode(Data(raw.utf8)), raw) {
                XCTAssertEqual(($0 as? RealtimeError)?.eventType, type, raw)
            }
        }
    }
}
#endif
