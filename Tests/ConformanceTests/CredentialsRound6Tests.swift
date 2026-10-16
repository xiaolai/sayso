#if os(macOS)
@testable import CodexAuth
import Foundation
import XCTest

/// The file source's member-only decoding after its verification: a needed member of the wrong kind is told from its
/// first byte and refused. That nothing of it is copied is AuthFileWipeTests'.
final class CredentialsRound6Tests: XCTestCase {
    /// auth_mode, tokens and access_token as an object, an array and a number, an unused credential nested in the
    /// first two: each keeps its codex-error state.
    func testNonStringMembersAreNotDecoded() {
        let canary = "nested-unused-credential-\(UUID().uuidString)"
        // A token read is registered with the process-wide scrubber, so it is unique and long.
        let access = "round6-token-\(UUID().uuidString)"
        let nested = [#"{"refresh_token":"\#(canary)"}"#, #"["\#(canary)"]"#, "7"]
        var cases: [String] = []
        for v in nested {
            cases.append(#"{"auth_mode":\#(v),"tokens":{"access_token":"\#(access)"}}"#)
            cases.append(#"{"auth_mode":"chatgpt","tokens":{"access_token":\#(v)}}"#)
            cases.append(#"{"auth_mode":"chatgpt","tokens":\#(v)}"#)
            cases.append(#"{"tokens":{"access_token": \#(v)}}"#)  // the value after a space
        }
        for json in cases {
            do {
                _ = try AuthFile.decode(Data(json.utf8))
                XCTFail("\(json): read a token")
            } catch let e as CodexAuthError {
                XCTAssertEqual(e.state, .codexError, json)
            } catch {
                XCTFail("\(json): \(error)")
            }
        }
    }
}
#endif
