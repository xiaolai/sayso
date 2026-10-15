#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import RealtimeConnection
import XCTest

/// Process lifetime, environment and byte-level comparisons the credentials fixtures cannot observe.
final class CredentialsRound3Tests: XCTestCase {
    /// A python child: it answers initialize, then runs `then` (Python source).
    private func fakeCodex(_ then: String) throws -> (String, () -> Void) {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let dir = NSTemporaryDirectory() + "sayso-r3-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let script = dir + "/codex"
        let body = "#!\(python)\nimport sys, json, time, signal\nm = json.loads(sys.stdin.readline())\n"
            + "print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n" + then
        FileManager.default.createFile(atPath: script, contents: Data(body.utf8))
        chmod(script, 0o755)
        return (script, { try? FileManager.default.removeItem(atPath: dir) })
    }

    /// A name holding "=" would become another variable in the child, past the removal list; a NUL would cut a
    /// string short. Both are refused before anything starts.
    func testEnvironmentThatCannotBePassedOnIsRefused() {
        for (env, home) in [
            (["OPENAI_API_KEY=canary": "x"], nil), (["X": "a\u{0}b"], nil), (["X\u{0}": "a"], nil), ([:], "/h\u{0}x"),
        ] as [([String: String], String?)] {
            let started = Locked(false)
            let cfg = AppServerClient.Configuration(
                bin: "/nonexistent/codex", codexHome: home, environment: env, timeout: 1,
                guardStart: { _ in started.set(true) })
            XCTAssertThrowsError(try AppServerClient.start(cfg))
            XCTAssertFalse(started.get(), "\(env.keys.sorted()) reached the start")
        }
    }

    /// Credentials compare as bytes, as in Go: canonically equivalent spellings are different credentials.
    func testCredentialsCompareAsBytes() {
        let a = AuthStatus(method: "chatgpt", requiresOpenAIAuth: true, token: "tok-\u{E9}")
        let b = AuthStatus(method: "chatgpt", requiresOpenAIAuth: true, token: "tok-e\u{301}")
        XCTAssertNotEqual(a, b)
        XCTAssertEqual(a, AuthStatus(method: "chatgpt", requiresOpenAIAuth: true, token: "tok-\u{E9}"))
        XCTAssertNotEqual(
            AuthStatus(method: "", requiresOpenAIAuth: true, token: ""),
            AuthStatus(method: nil, requiresOpenAIAuth: true, token: ""))
        XCTAssertFalse(sameBearer("tok-\u{E9}", "tok-e\u{301}"))
        XCTAssertTrue(sameBearer(nil, nil))
    }

    /// Versions and home folders are read as Go reads them.
    func testVersionsAndCandidatesAsGoReadsThem() {
        XCTAssertFalse(CodexExecutable.versionLess("1.\u{301}0", "0.162.0"), "a dot before a combining mark")
        XCTAssertTrue(CodexExecutable.versionLess("0.161.9", "0.162.0"))
        XCTAssertEqual(Credentials.version("codex_cli_rs/0.162\u{301}.1 (x)"), "0.162")
        XCTAssertEqual(CodexExecutable.candidates(home: ""), ["/opt/homebrew/bin", "/usr/local/bin"])
    }

    /// The whole chain of underlying errors is followed.
    func testStateFollowsADeepChain() {
        var e: Error = CodexAuthError(.expired, message: "m")
        for i in 0..<40 { e = NSError(domain: "x", code: i, userInfo: [NSUnderlyingErrorKey: e]) }
        XCTAssertEqual(CodexAuthError.state(of: e), .expired)
        XCTAssertEqual(CodexAuthError.state(of: NSError(domain: "x", code: 1)), .codexError)
    }

    /// Fields are read-only outside the package and sanitised on every assignment inside it.
    func testErrorFieldsStaySanitised() {
        Secret.register("opaque-canary-r3-1")
        var e = CodexAuthError(.unsupportedAuth, mode: "m")
        e.mode = "opaque-canary-r3-1\u{7}"
        e.message = "ek_canary_r3"
        e.cause = "opaque-canary-r3-1"
        var dumped = ""
        dump(e, to: &dumped)
        XCTAssertFalse(dumped.contains("canary"), "a field set after construction was stored as given")
    }

    /// Parameters nested deeper than Codex's JSON may be are refused before the encoder, which cannot be caught.
    func testParametersNestedTooDeepAreRefused() throws {
        let (script, cleanup) = try fakeCodex("time.sleep(30)\n")
        defer { cleanup() }
        let c = try AppServerClient.start(
            AppServerClient.Configuration(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3))
        defer { c.close() }
        var deep: Any = "x"
        for _ in 0..<200 { deep = ["a": deep] }
        let t0 = Date()
        XCTAssertThrowsError(try c.call("x", params: ["p": deep]))
        // Refused before it is sent; a request that was sent would wait the 3 s timeout for the silent child.
        XCTAssertLessThan(Date().timeIntervalSince(t0), 1, "the request was sent")
    }

    /// A child that exited stays unreaped until close, so its group ID cannot be reused while close may signal it.
    func testExitedChildStaysUnreapedUntilClose() throws {
        let (script, cleanup) = try fakeCodex("sys.stdin.readline()\n")
        defer { cleanup() }
        let c = try AppServerClient.start(
            AppServerClient.Configuration(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3))
        let pid = c.pid
        let deadline = Date().addingTimeInterval(5)
        while !c.hasExited && Date() < deadline { usleep(20_000) }
        XCTAssertTrue(c.hasExited, "the child did not exit after initialized")
        XCTAssertEqual(kill(pid, 0), 0, "the exited child was reaped before close")
        c.close()
        XCTAssertEqual(kill(pid, 0), -1)
        XCTAssertEqual(errno, ESRCH, "close left the child unreaped")
    }

    /// A second close waits for the first, as Go's sync.Once makes it.
    func testConcurrentCloseWaitsForTheFirst() throws {
        let (script, cleanup) = try fakeCodex("signal.signal(signal.SIGTERM, signal.SIG_IGN)\ntime.sleep(30)\n")
        defer { cleanup() }
        let c = try AppServerClient.start(
            AppServerClient.Configuration(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3))
        let first = Thread { c.close() }
        first.start()
        usleep(200_000)
        let t0 = Date()
        c.close()
        XCTAssertGreaterThan(Date().timeIntervalSince(t0), 1, "a second close returned while the first still ran")
        XCTAssertTrue(c.hasExited)
    }
}

private final class Locked<T: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: T
    init(_ v: T) { value = v }
    func get() -> T { lock.withLock { value } }
    func set(_ v: T) { lock.withLock { value = v } }
}
#endif
