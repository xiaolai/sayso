#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import RealtimeConnection
import XCTest

/// Error hygiene, paths, the refusal bookkeeping and the write path under failure: what the credentials fixtures
/// cannot observe.
final class CredentialsBoundaryTests: XCTestCase {
    private func dumped(_ v: Any) -> String {
        var s = ""
        dump(v, to: &s)
        return s + String(reflecting: v) + "\(v)"
    }

    func testAppServerErrorIsScrubbedAndReflectsNoRPCMessage() {
        Secret.register("opaque-canary-rpc-1")
        let e = AppServerError("rejected ek_canarymessage", rpcCode: -32000, rpcMessage: "opaque-canary-rpc-1 and more")
        XCTAssertFalse(e.message.contains("canary"))
        XCTAssertFalse(dumped(e).contains("canary"), "an AppServerError shows a secret or its rpcMessage")
        Secret.register("opaque-canary-later-2")
        let later = AppServerError("x"), named = AppServerError("y opaque-canary-later-2")
        XCTAssertFalse(named.description.contains("canary"))
        XCTAssertEqual(later.description, "x")
    }

    /// A failed start's stderr reaches the caller through `stderr` only, unscrubbed formats included; the exit status
    /// is part of the description, written as Go writes it.
    func testStartStderrIsNeverPrinted() {
        let canary = "start-stderr-canary-unregistered"
        var e = AppServerError("Codex app-server exited unexpectedly")
        e.exitStatus = "exit status 3"
        e.capturedStderr = canary
        XCTAssertEqual(e.stderr, canary)
        XCTAssertEqual(e.description, "Codex app-server exited unexpectedly (exit status 3)")
        XCTAssertFalse(dumped(e).contains("canary"), "an AppServerError prints its stderr")
        var c = CodexAuthError(.codexError, message: "Codex app-server did not start", cause: e.description)
        c.capturedStderr = e.stderr
        XCTAssertEqual(c.stderr, canary)
        XCTAssertFalse(dumped(c).contains("canary"), "a CodexAuthError prints its stderr")
        XCTAssertTrue(c.description.contains("(exit status 3)"))
        // The wait status, as waitpid reports it: the exit code in the second byte, a signal in the low seven bits.
        XCTAssertEqual(AppServerCore.describe(waitStatus: 3 << 8), "exit status 3")
        XCTAssertEqual(AppServerCore.describe(waitStatus: 0), "exit status 0")
        XCTAssertEqual(AppServerCore.describe(waitStatus: SIGKILL), "signal 9")
        XCTAssertEqual(AppServerCore.describe(waitStatus: SIGABRT | 0x80), "signal 6")  // with a core dump
    }

    func testCodexAuthErrorSanitisesEveryField() {
        let e = CodexAuthError(.unsupportedAuth, mode: "e\u{0}k_canarymode\u{7}", codexVersion: "ek_canaryversion")
        XCTAssertFalse(e.mode.contains("canary"), "the mode was stored unfiltered")
        XCTAssertFalse(e.codexVersion.contains("canary"), "the version was stored unscrubbed")
        XCTAssertFalse(dumped(e).contains("canary"))
        XCTAssertEqual(CodexAuthError(.unsupportedAuth, mode: String(repeating: "m", count: 80)).mode.count, 64)
    }

    func testStateOfFollowsUnderlyingErrors() {
        let inner = CodexAuthError(.expired, message: "x")
        let wrapped = NSError(domain: "app", code: 1, userInfo: [NSUnderlyingErrorKey: inner as NSError])
        XCTAssertEqual(CodexAuthError.state(of: wrapped), .expired)
        XCTAssertEqual(CodexAuthError.state(of: NSError(domain: "app", code: 2)), .codexError)
        XCTAssertEqual(CodexAuthError.state(of: nil), .ready)
    }

    func testPathsCleanLikeGo() {
        XCTAssertEqual(clean("../user/.codex/auth.json"), "../user/.codex/auth.json")
        XCTAssertEqual(clean("a/../../b"), "../b")
        XCTAssertEqual(clean("/../a"), "/a")
        XCTAssertEqual(clean("/opt/\u{301}node/../bin/codex"), "/opt/bin/codex")
        XCTAssertEqual(clean(""), ".")
        XCTAssertEqual(dirname("/opt/\u{301}bin"), "/opt")
        XCTAssertEqual(join("../user", ".codex/auth.json"), "../user/.codex/auth.json")
    }

    func testNULIsRefusedBeforeJoining() throws {
        let root = NSTemporaryDirectory() + "sayso-nul-\(UUID().uuidString)"
        let dir = root + "/trusted"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: root) }
        FileManager.default.createFile(atPath: dir + "/codex", contents: Data("#!/bin/sh\n".utf8))
        chmod(dir + "/codex", 0o755)
        XCTAssertNil(CodexExecutable.resolve(explicit: nil, path: root + "/bad\u{0}/../trusted", candidates: []))
        XCTAssertNil(CodexExecutable.resolve(explicit: nil, path: "", candidates: [root + "/bad\u{0}/../trusted"]))
        XCTAssertNotNil(CodexExecutable.resolve(explicit: nil, path: dir, candidates: []))
        XCTAssertEqual(CodexExecutable.candidates(home: root + "\u{0}/.."), ["/opt/homebrew/bin", "/usr/local/bin"])
    }

    func testVersionComponents() {
        XCTAssertTrue(CodexExecutable.versionLess("0.999999999999999999999x", "0.162.0"), "a non-numeric part is 0")
        XCTAssertFalse(CodexExecutable.versionLess("0.999999999999999999999", "0.162.0"), "an overflow saturates")
        XCTAssertTrue(CodexExecutable.versionLess("3000000000", "4000000000"))
    }

    private func jwt(_ exp: Int) -> String {
        func b(_ s: String) -> String {
            Data(s.utf8).base64EncodedString().replacingOccurrences(of: "+", with: "-")
                .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
        }
        return b(#"{"alg":"none"}"#) + "." + b(#"{"exp":\#(exp)}"#) + ".c2ln"
    }

    /// A late refusal of an older token does not unmark the current one: after A was replaced by B, B was refused and
    /// its refresh failed, a delayed refusal of A must not let token() hand out B.
    func testLateRefusalKeepsTheCurrentRefusal() async throws {
        let now = Date(timeIntervalSince1970: 1_800_000_000)
        let creds = try Credentials(
            CodexAuthConfiguration(margin: 60, timeout: 1, source: .codex, environment: [:], now: { now }))
        let a = Token(bearer: jwt(1_800_003_600) + "a", expires: Date(timeIntervalSince1970: 1_800_003_600))
        let b = Token(bearer: jwt(1_800_003_601) + "b", expires: Date(timeIntervalSince1970: 1_800_003_601))
        creds.seedForTesting(a, refreshable: true)
        let calls = Counter()
        creds.refresher = {
            calls.increment()
            if calls.value == 1 { return b }
            throw CodexAuthError(.codexError, message: "offline")
        }
        let got = try await creds.refresh(after: a)
        XCTAssertEqual(got.bearer, b.bearer)
        do {
            _ = try await creds.refresh(after: b)
            XCTFail("B's refresh succeeded")
        } catch {}
        do {
            _ = try await creds.refresh(after: a)  // late: A was refused long ago
            XCTFail("a late refusal of A returned a token")
        } catch {}
        do {
            let t = try await creds.token()
            XCTFail("token() handed out \(t.bearer == b.bearer ? "the refused B" : "a token")")
        } catch {}
        XCTAssertEqual(calls.value, 2, "the late refusal refreshed within the backoff")
    }

    /// A refresh reply naming another sign-in is that sign-in's state, not expired.
    func testRefreshReplyKeepsItsSignInState() async throws {
        let creds = try Credentials(CodexAuthConfiguration(margin: 60, timeout: 1, source: .codex, environment: [:]))
        let tok = Token(bearer: "opaque-mode-change", expires: nil)
        creds.seedForTesting(tok, refreshable: true)
        creds.refresher = { throw CodexAuthError(.apiKey, mode: "apikey", message: "now an API key") }
        do {
            _ = try await creds.refresh(after: tok)
            XCTFail("refresh succeeded")
        } catch {
            XCTAssertEqual(CodexAuthError.state(of: error), .apiKey)
        }
    }

    // MARK: - The write path, against a child that answers initialize and never reads again

    private func stalledChild(answersInitialize: Bool = true) throws -> (String, () -> Void) {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let dir = NSTemporaryDirectory() + "sayso-stall2-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let script = dir + "/codex"
        let reply =
            answersInitialize
            ? "m = json.loads(sys.stdin.readline())\nprint(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n"
            : ""
        let body = "#!\(python)\nimport sys, json, time\n\(reply)time.sleep(30)\n"
        FileManager.default.createFile(atPath: script, contents: Data(body.utf8))
        chmod(script, 0o755)
        return (script, { try? FileManager.default.removeItem(atPath: dir) })
    }

    /// Parameters JSONSerialization cannot encode are refused, never raised as an Objective-C exception.
    func testUnencodableParametersThrow() throws {
        let (script, cleanup) = try stalledChild()
        defer { cleanup() }
        let c = try AppServerClient.start(.init(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3))
        defer { c.close() }
        XCTAssertThrowsError(try c.call("x", params: ["when": Date()]))
        XCTAssertThrowsError(try c.call("x", params: ["n": Double.nan]))
        XCTAssertThrowsError(try c.call("x", params: ["nested": [["d": Data()]]]))
    }

    /// A write cut off inside a request leaves the stream unusable: the next call fails at once instead of
    /// appending to a half-written line. A cancellation ends a blocked write promptly.
    func testCutOffWriteFailsTheClientAndCancelEndsAWrite() throws {
        let (script, cleanup) = try stalledChild()
        defer { cleanup() }
        let cfg = AppServerClient.Configuration(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3)
        let c = try AppServerClient.start(cfg)
        XCTAssertThrowsError(try c.call("big", params: ["pad": String(repeating: "x", count: 4 << 20)]))
        let t0 = Date()
        XCTAssertThrowsError(try c.call("small"))
        XCTAssertLessThan(Date().timeIntervalSince(t0), 1, "a call after a cut-off write waited instead of failing")
        c.close()

        let d = try AppServerClient.start(cfg)
        defer { d.close() }
        let done = DispatchSemaphore(value: 0)
        let started = Date()
        Thread.detachNewThread {
            _ = try? d.call("big", params: ["pad": String(repeating: "x", count: 4 << 20)])
            done.signal()
        }
        usleep(300_000)
        d.cancel()
        XCTAssertEqual(done.wait(timeout: .now() + 1.5), .success, "cancel did not end the blocked write")
        XCTAssertLessThan(Date().timeIntervalSince(started), 2.5)
    }

    /// A cancellation during the initialize handshake ends it, rather than waiting out the timeout.
    func testCancelEndsTheInitializeHandshake() async throws {
        let (script, cleanup) = try stalledChild(answersInitialize: false)
        defer { cleanup() }
        let creds = try Credentials(
            CodexAuthConfiguration(
                margin: 60, timeout: 20, source: .codex, executable: script, environment: ["PATH": "/usr/bin:/bin"]))
        let t0 = Date()
        let task = Task { try await creds.token() }
        try await Task.sleep(nanoseconds: 500_000_000)
        task.cancel()
        do {
            _ = try await task.value
            XCTFail("a cancelled handshake succeeded")
        } catch {
            XCTAssertEqual(ErrorClass.of(error), .cancelled, "\(error)")
        }
        XCTAssertLessThan(Date().timeIntervalSince(t0), 5, "the handshake ran on after the cancellation")
    }
}
#endif
