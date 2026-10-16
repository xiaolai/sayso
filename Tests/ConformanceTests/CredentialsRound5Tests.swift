#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
@testable import RealtimeConnection
import XCTest

/// The file source and the error type after the audit of their round-4 changes.
final class CredentialsRound5Tests: XCTestCase {
    private func tempBase() throws -> String {
        try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
    }

    private func writeAuth(_ dir: String, _ body: String) {
        FileManager.default.createFile(
            atPath: dir + "/auth.json", contents: Data(body.utf8), attributes: [.posixPermissions: 0o600])
    }

    /// An ancestor the user may search but not read, as a 0711 home folder is to others, does not refuse CODEX_HOME:
    /// walking a path needs search permission only. 0311 on the user's own folder stands in for another user's.
    func testSearchableUnreadableAncestorIsWalked() throws {
        let home = try tempBase() + "/sayso-search-\(UUID().uuidString)"
        let codexHome = home + "/a/codex"
        try FileManager.default.createDirectory(atPath: codexHome, withIntermediateDirectories: true)
        writeAuth(codexHome, #"{"tokens":{"access_token":"searched-token"}}"#)
        XCTAssertEqual(chmod(home + "/a", 0o311), 0)
        defer {
            chmod(home + "/a", 0o700)
            try? FileManager.default.removeItem(atPath: home)
        }
        guard (try? FileManager.default.contentsOfDirectory(atPath: home + "/a")) == nil else {
            throw XCTSkip("the folder is readable anyway")
        }
        let loc = try AuthFile.location(codexHome: codexHome, home: home, confine: home)
        defer { loc.close() }
        XCTAssertEqual(try AuthFile.read(loc, onRetry: nil).bearer, "searched-token")
    }

    /// An open interrupted by a signal is retried, for each folder of the walk and for the file, instead of turning
    /// a usable file into codex-error.
    func testInterruptedOpensAreRetried() throws {
        let home = try tempBase() + "/sayso-eintr-\(UUID().uuidString)"
        let codexHome = home + "/codex"
        try FileManager.default.createDirectory(atPath: codexHome, withIntermediateDirectories: true)
        defer {
            AuthFile.interruptions.set(0)
            try? FileManager.default.removeItem(atPath: home)
        }
        writeAuth(codexHome, #"{"tokens":{"access_token":"eintr-token"}}"#)
        AuthFile.interruptions.set(5)
        let loc = try AuthFile.location(codexHome: codexHome, home: home, confine: home)
        defer { loc.close() }
        AuthFile.interruptions.set(3)
        XCTAssertEqual(try AuthFile.read(loc, onRetry: nil).bearer, "eintr-token")
        XCTAssertFalse(AuthFile.interruptions.take(), "not every injected interruption was met")
    }

    /// Only the members the file source needs are read: a value it never uses cannot make the file unreadable, the
    /// mode keys still decide, and a tokens value that is not an object holds no access token.
    func testOnlyTheNeededMembersAreDecoded() throws {
        // A token read is registered with the process-wide scrubber, so it is unique and long: "t" would then be
        // removed from every later message in the run.
        let access = "member-only-token-\(UUID().uuidString)"
        func decode(_ json: String) throws -> Token? {
            try AuthFile.decode(Data(json.replacingOccurrences(of: "\"TOKEN\"", with: "\"\(access)\"").utf8))
        }
        XCTAssertEqual(
            try decode(#"{"tokens":{"access_token":"TOKEN","refresh_token":"r","id_token":{"x":[1,2]}},"last_refresh":7}"#)?
                .bearer, access)
        XCTAssertNil(try decode(#"["not","an","object"]"#), "a document that is no object is not a sign-in file")
        for (json, state) in [
            (#"{"OPENAI_API_KEY":"sk-x","tokens":{"access_token":"TOKEN"}}"#, CodexAuthState.apiKey),
            (#"{"OPENAI_API_KEY":null,"tokens":{"access_token":"TOKEN"}}"#, .ready),
            (#"{"auth_mode":"headers","tokens":{"access_token":"TOKEN"}}"#, .unsupportedAuth),
            (#"{"auth_mode":7,"tokens":{"access_token":"TOKEN"}}"#, .codexError),
            (#"{"tokens":"TOKEN"}"#, .codexError),
            (#"{"tokens":{"access_token":""}}"#, .codexError),
            (#"{"OPENAI_API_KEY":"sk-x","tokens":{"access_token":"TOKEN"}}"#, .ready),  // a Kelvin sign: names match as bytes
        ] {
            do {
                _ = try decode(json)
                XCTAssertEqual(state, .ready, json)
            } catch let e as CodexAuthError {
                XCTAssertEqual(e.state, state, json)
            }
        }
    }
}
#endif
