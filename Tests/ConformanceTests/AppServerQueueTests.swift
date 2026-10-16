#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import XCTest

/// The queues the app-server client keeps, mirroring go/codexapp/outbound_test.go and lifecycle_test.go: what is
/// taken is let go at once, a failure drops what is queued, and the order is admission to the one outbound queue.
final class AppServerQueueTests: XCTestCase {
    private final class Payload {
        let n: Int
        init(_ n: Int) { self.n = n }
    }

    /// Each element is released the moment it is taken, not when the front is compacted later.
    func testFIFOReleasesEachElementWhenTaken() {
        var q = FIFO<Payload>()
        weak var taken: Payload?
        for i in 0..<3000 { q.append(Payload(i)) }
        for i in 0..<2000 {
            autoreleasepool {
                let p = q.popFirst()
                XCTAssertEqual(p?.n, i)
                taken = p
            }
            XCTAssertNil(taken, "element \(i) was kept after it was taken")
        }
        XCTAssertEqual(q.count, 1000)
        weak var last: Payload?
        autoreleasepool {
            let p = Payload(-1)
            last = p
            q.append(p)
        }
        q.removeAll()
        XCTAssertNil(last, "removeAll kept an element")
        XCTAssertEqual(q.count, 0)
        XCTAssertNil(q.popFirst())
    }

    /// A python child: it answers initialize, reads initialized, then runs `then` (Python source).
    private func fakeCodex(_ then: String) throws -> (String, () -> Void) {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let dir = NSTemporaryDirectory() + "sayso-queue-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let script = dir + "/codex"
        let body = "#!\(python)\nimport sys, json, time\nm = json.loads(sys.stdin.readline())\n"
            + "print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n"
            + "sys.stdin.readline()\n" + then
        FileManager.default.createFile(atPath: script, contents: Data(body.utf8))
        chmod(script, 0o755)
        return (script, { try? FileManager.default.removeItem(atPath: dir) })
    }

    private func start(_ script: String, timeout: TimeInterval, _ onEvent: (@Sendable (AppServerEvent) -> Void)?)
        throws -> AppServerClient
    {
        try AppServerClient.start(
            AppServerClient.Configuration(
                bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: timeout, onEvent: onEvent))
    }

    /// A failure drops the outbound queue: answers the child never read are not kept, and their charge is released.
    func testFailureDropsTheOutboundQueue() throws {
        // Ten answers of about 20 KB each are more than the child's input pipe holds; it never reads them, then breaks
        // its output.
        let (script, cleanup) = try fakeCodex(
            "call = json.loads(sys.stdin.readline())\nfor i in range(10):\n"
                + "    sys.stdout.write(json.dumps({'id': 'x' * 20000 + str(i), 'method': 'approve'}) + '\\n')\n"
                + "sys.stdout.write('not json\\n')\nsys.stdout.flush()\ntime.sleep(30)\n")
        defer { cleanup() }
        let c = try start(script, timeout: 5, nil)
        defer { c.close() }
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("malformed"), "\($0)") }
        let held = c.held
        XCTAssertEqual(held.outbound, 0, "messages kept after the failure")
        XCTAssertEqual(held.refusalBytes, 0, "refusal charge kept after the failure")
    }

}
#endif
