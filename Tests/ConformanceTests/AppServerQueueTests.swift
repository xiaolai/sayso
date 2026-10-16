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

    /// An answer admitted before a request is written ahead of it, and one admitted while the request's call waits is
    /// written while it waits. The child is the barrier: it sends srv-2 only after it has read the request, and
    /// answers the request only after it has read the answer to srv-2, reporting the order it read them in.
    func testCallBetweenEarlierAndLaterRefusals() throws {
        let (script, cleanup) = try fakeCodex(
            "print(json.dumps({'id': 'srv-1', 'method': 'approve'}), flush=True)\n"
                + "order = []\ncall = None\n"
                + "for line in sys.stdin:\n"
                + "    m = json.loads(line)\n"
                + "    order.append(str(m['id']) if 'error' in m else 'call')\n"
                + "    if 'method' in m:\n"
                + "        call = m\n"
                + "        print(json.dumps({'id': 'srv-2', 'method': 'approve'}), flush=True)\n"
                + "    if len(order) == 3:\n"
                + "        print(json.dumps({'id': call['id'], 'result': {'order': order}}), flush=True)\n"
                + "time.sleep(30)\n")
        defer { cleanup() }
        let firstRequest = DispatchSemaphore(value: 0)
        // An event for a server request is delivered after its answer was queued: the call starts after that.
        let c = try start(script, timeout: 5) { e in
            if e.isServerRequest && e.method == "approve" { firstRequest.signal() }
        }
        defer { c.close() }
        XCTAssertEqual(firstRequest.wait(timeout: .now() + 5), .success)
        let r = try XCTUnwrap(try c.call("B") as? [String: Any])
        XCTAssertEqual(r["order"] as? [String], ["srv-1", "call", "srv-2"])
    }

    /// Calls from many threads contend for the writer while the child sends server requests between its replies:
    /// every call returns, and the child reads every answer exactly once.
    func testConcurrentCallsWithInterleavedRefusals() throws {
        let (script, cleanup) = try fakeCodex(
            "seen = set()\nrefused = set()\nn = 0\n"
                + "for line in sys.stdin:\n"
                + "    m = json.loads(line)\n"
                + "    if 'error' in m:\n"
                + "        assert m['id'] not in refused\n"
                + "        refused.add(m['id'])\n"
                + "        continue\n"
                + "    if m.get('method') == 'count':\n"
                + "        print(json.dumps({'id': m['id'], 'result': {'refused': len(refused), 'calls': n}}), flush=True)\n"
                + "        continue\n"
                + "    n += 1\n"
                + "    print(json.dumps({'id': 'srv-%d' % n, 'method': 'approve'}), flush=True)\n"
                + "    print(json.dumps({'id': m['id'], 'result': {}}), flush=True)\n")
        defer { cleanup() }
        let c = try start(script, timeout: 10, nil)
        defer { c.close() }
        let group = DispatchGroup()
        let failures = NSLock()
        nonisolated(unsafe) var errors: [String] = []
        for _ in 0..<24 {
            group.enter()
            Thread {
                defer { group.leave() }
                do { _ = try c.call("echo") } catch { failures.withLock { errors.append("\(error)") } }
            }.start()
        }
        XCTAssertEqual(group.wait(timeout: .now() + 20), .success, "a call did not return")
        XCTAssertEqual(failures.withLock { errors }, [])
        // The answers to the last server requests may still be on their way: count until all 24 have arrived.
        var refused = 0
        let deadline = Date().addingTimeInterval(5)
        repeat {
            let r = try XCTUnwrap(try c.call("count") as? [String: Any])
            XCTAssertEqual(r["calls"] as? Int, 24)
            refused = r["refused"] as? Int ?? -1
        } while refused < 24 && Date() < deadline
        XCTAssertEqual(refused, 24)
    }

}
#endif
