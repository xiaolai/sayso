#if os(macOS)
@testable import CodexAuth
import Foundation
import XCTest

/// The file source's member-only decoding after its verification: a needed member that is not a string is told
/// from its bytes, never decoded, so a credential nested in it never becomes a Foundation string.
final class CredentialsRound6Tests: XCTestCase {
    /// auth_mode, tokens and access_token as an object, an array and a number, an unused credential nested in the
    /// first two: each keeps its codex-error state, and the nested value never reaches the Foundation decoder.
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
        defer { _ = AuthFile.decoded.take() }
        for json in cases {
            AuthFile.decoded.watch(canary)
            do {
                _ = try AuthFile.decode(Data(json.utf8))
                XCTFail("\(json): read a token")
            } catch let e as CodexAuthError {
                XCTAssertEqual(e.state, .codexError, json)
            } catch {
                XCTFail("\(json): \(error)")
            }
            XCTAssertFalse(AuthFile.decoded.take(), "\(json): the nested credential was decoded")
        }
    }

    /// The watch sees what reaches the decoder: a string member is decoded, and an unknown string mode keeps its
    /// state.
    func testTheWatchSeesADecodedString() {
        let canary = "watched-mode-\(UUID().uuidString)"
        AuthFile.decoded.watch(canary)
        defer { _ = AuthFile.decoded.take() }
        XCTAssertThrowsError(try AuthFile.decode(Data(#"{"auth_mode":"\#(canary)"}"#.utf8))) {
            XCTAssertEqual(($0 as? CodexAuthError)?.state, .codexError)
        }
        XCTAssertTrue(AuthFile.decoded.take(), "a string member did not reach the decoder")
    }
}
#endif
