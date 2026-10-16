#if os(macOS)
import CodexAuth
import Darwin
import Foundation
import XCTest

/// The bounds on held events, mirroring go/codexapp/lifecycle_test.go.
/// Barriers, not sleeps, order the steps.
final class AppServerLifecycleTests: XCTestCase {
    /// A python child: it answers initialize, reads initialized, then runs `then` (Python source).
    private func fakeCodex(_ then: String) throws -> (String, () -> Void) {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let dir = NSTemporaryDirectory() + "sayso-life-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let script = dir + "/codex"
        let body = "#!\(python)\nimport sys, json, time\nm = json.loads(sys.stdin.readline())\n"
            + "print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n"
            + "sys.stdin.readline()\n" + then
        FileManager.default.createFile(atPath: script, contents: Data(body.utf8))
        chmod(script, 0o755)
        return (script, { try? FileManager.default.removeItem(atPath: dir) })
    }

    /// Records the methods its handler sees; the handler of the first event blocks until `release` is signalled.
    private final class Recorder: @unchecked Sendable {
        private let lock = NSLock()
        private var methods: [String] = []
        let entered = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)

        func handle(_ e: AppServerEvent) {
            let first = lock.withLock { () -> Bool in
                methods.append(e.method)
                return methods.count == 1
            }
            if first {
                entered.signal()
                release.wait()
            }
        }

        var seen: [String] { lock.withLock { methods } }
    }

    private func start(_ script: String, timeout: TimeInterval, _ onEvent: (@Sendable (AppServerEvent) -> Void)?)
        throws -> AppServerClient
    {
        try AppServerClient.start(
            AppServerClient.Configuration(
                bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: timeout, onEvent: onEvent))
    }

    /// Events beyond the count bound fail the client with the overflow, even when each is empty, and a reply the
    /// child writes after them is not handed out. Later calls fail at once. The timeout leaves a debug build on a
    /// loaded machine time to read 70,000 lines: the bound, not the speed, is under test.
    func testEmptyEventsTripTheBounds() throws {
        let (script, cleanup) = try fakeCodex(
            "call = json.loads(sys.stdin.readline())\nsys.stdout.write((json.dumps({'method': ''}) + '\\n') * 70000)\n"
                + "sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"
                + "sys.stdout.flush()\ntime.sleep(30)\n")
        defer { cleanup() }
        let r = Recorder()
        let c = try start(script, timeout: 60) { r.handle($0) }
        defer {
            c.close()
            r.release.signal()
        }
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("faster than the handler"), "\($0)") }
        let t0 = Date()
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("faster than the handler"), "\($0)") }
        XCTAssertLessThan(Date().timeIntervalSince(t0), 0.1, "a later call did not fail at once")
    }

    /// The bounds charge the event being handled too, so the boundary is exact: 65,536 events are held, one more
    /// fails the client.
    func testEventCountBoundIsExact() throws {
        for (n, fails) in [(65_536, false), (65_537, true)] {
            let (script, cleanup) = try fakeCodex(
                "call = json.loads(sys.stdin.readline())\nsys.stdout.write((json.dumps({'method': ''}) + '\\n') * \(n))\n"
                    + "sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"
                    + "sys.stdout.flush()\ntime.sleep(30)\n")
            defer { cleanup() }
            let r = Recorder()
            let c = try start(script, timeout: 60) { r.handle($0) }
            let failed = (try? c.call("echo")) == nil
            XCTAssertEqual(failed, fails, "\(n) events")
            c.close()
            r.release.signal()
        }
    }
}
#endif
