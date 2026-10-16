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
}
#endif
