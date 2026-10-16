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

    /// A client closed as soon as it starts keeps nothing queued, and a later call fails.
    func testImmediateClose() throws {
        let (script, cleanup) = try fakeCodex("for line in sys.stdin:\n    pass\n")
        defer { cleanup() }
        let c = try start(script, timeout: 5) { _ in }
        c.close()
        let held = c.held
        XCTAssertEqual(held.outbound + held.events + held.refusalBytes + held.eventBytes, 0, "kept after close: \(held)")
        XCTAssertThrowsError(try c.call("echo"))
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
            "seen = set()\nrefused = set()\nn = 0\norder = []\n"
                + "for line in sys.stdin:\n"
                + "    m = json.loads(line)\n"
                + "    order.append(str(m['id']))\n"
                + "    if 'error' in m:\n"
                + "        assert m['id'] not in refused\n"
                + "        refused.add(m['id'])\n"
                + "        continue\n"
                + "    if m.get('method') == 'count':\n"
                + "        print(json.dumps({'id': m['id'], 'result': {'refused': len(refused), 'calls': n, 'order': order}}), flush=True)\n"
                + "        continue\n"
                + "    n += 1\n"
                + "    print(json.dumps({'id': 'srv-%d' % n, 'method': 'approve'}), flush=True)\n"
                + "    print(json.dumps({'id': m['id'], 'result': {}}), flush=True)\n")
        defer { cleanup() }
        let c = try start(script, timeout: 10, nil)
        defer { c.close() }
        c.setTestSeams(recordAdmissions: true)
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
        var order: [String] = []
        let deadline = Date().addingTimeInterval(5)
        repeat {
            let r = try XCTUnwrap(try c.call("count") as? [String: Any])
            XCTAssertEqual(r["calls"] as? Int, 24)
            refused = r["refused"] as? Int ?? -1
            order = r["order"] as? [String] ?? []
        } while refused < 24 && Date() < deadline
        XCTAssertEqual(refused, 24)
        // The child read every message in the order it was admitted to the one queue, not merely all of them.
        let admitted = c.admittedMessages.map { data -> String in
            guard let m = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any], let id = m["id"] else {
                return "?"
            }
            return "\(id)"
        }
        XCTAssertEqual(Array(admitted.prefix(order.count)), order, "read out of admission order")
    }

    /// An answer is admitted when its server request is read, before it is encoded: a request queued while the answer
    /// is still being encoded is written after it, and the writer waits at the answer's slot meanwhile. The child sends
    /// srv-1, then reports the order it reads the next two messages in.
    func testAnswerIsAdmittedBeforeItIsEncoded() throws {
        let (script, cleanup) = try fakeCodex(
            "arm = json.loads(sys.stdin.readline())\n"
                + "print(json.dumps({'id': arm['id'], 'result': {}}), flush=True)\n"
                + "print(json.dumps({'id': 'srv-1', 'method': 'approve'}), flush=True)\n"
                + "order = []\ncall = None\n"
                + "for line in sys.stdin:\n"
                + "    m = json.loads(line)\n"
                + "    order.append(str(m['id']) if 'error' in m else 'call')\n"
                + "    if 'method' in m:\n"
                + "        call = m\n"
                + "    if len(order) == 2:\n"
                + "        print(json.dumps({'id': call['id'], 'result': {'order': order}}), flush=True)\n"
                + "time.sleep(30)\n")
        defer { cleanup() }
        let admitted = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)
        let c = try start(script, timeout: 5, nil)
        defer { c.close() }
        c.setTestSeams(refusalAdmitted: {
            admitted.signal()
            release.wait()
        })
        _ = try c.call("arm")  // after its reply the child sends srv-1
        XCTAssertEqual(admitted.wait(timeout: .now() + 5), .success, "srv-1 was not answered")
        let finished = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var reply: Any?
        Thread {
            reply = try? c.call("B")
            finished.signal()
        }.start()
        // The call is queued behind the answer's slot, not written past it.
        let deadline = Date().addingTimeInterval(5)
        while c.held.outbound != 2 && Date() < deadline { Thread.sleep(forTimeInterval: 0.002) }
        XCTAssertEqual(c.held.outbound, 2, "the call was not held behind the answer being encoded")
        release.signal()
        // reply is read only once the worker has finished writing it.
        guard finished.wait(timeout: .now() + 5) == .success else {
            XCTFail("the call did not return")
            return
        }
        XCTAssertEqual((reply as? [String: Any])?["order"] as? [String], ["srv-1", "call"])
    }

    /// A request admitted before a server request is read is written before that request's answer, even while it is
    /// still waiting to be written. Call A fills the pipe and holds the writer, and call B waits behind it unwritten.
    /// Only then does the child send srv-1, and it reads nothing until the answer has been admitted, so the answer
    /// meets B still queued. The child reads A, B, then the answer to srv-1.
    func testLaterAnswerDoesNotOvertakeAQueuedRequest() throws {
        let send = NSTemporaryDirectory() + "sayso-send-\(UUID().uuidString)"
        let drain = NSTemporaryDirectory() + "sayso-drain-\(UUID().uuidString)"
        defer {
            try? FileManager.default.removeItem(atPath: send)
            try? FileManager.default.removeItem(atPath: drain)
        }
        let (script, cleanup) = try fakeCodex(
            "import os\n"
                + "while not os.path.exists('\(send)'): time.sleep(0.005)\n"
                + "print(json.dumps({'id': 'srv-1', 'method': 'approve'}), flush=True)\n"
                + "while not os.path.exists('\(drain)'): time.sleep(0.005)\n"
                + "order = []\ncalls = []\n"
                + "for line in sys.stdin:\n"
                + "    m = json.loads(line)\n"
                + "    if 'error' in m:\n"
                + "        order.append(str(m['id']))\n"
                + "    else:\n"
                + "        order.append(m['method'])\n"
                + "        calls.append(m)\n"
                + "    if len(order) == 3:\n"
                + "        for c in calls: print(json.dumps({'id': c['id'], 'result': {'order': order}}), flush=True)\n"
                + "time.sleep(30)\n")
        defer { cleanup() }
        let c = try start(script, timeout: 20, nil)
        defer { c.close() }
        let answerAdmitted = DispatchSemaphore(value: 0)
        c.setTestSeams(refusalAdmitted: { answerAdmitted.signal() }, recordAdmissions: true)
        let group = DispatchGroup()
        let lock = NSLock()
        nonisolated(unsafe) var replies: [String: [String]] = [:]
        func waitFor(_ what: String, _ done: () -> Bool) {
            let deadline = Date().addingTimeInterval(5)
            while !done() && Date() < deadline { Thread.sleep(forTimeInterval: 0.002) }
            XCTAssertTrue(done(), what)
        }
        // A's 1 MiB line is far beyond the pipe buffer, and the child reads nothing until told to drain.
        for (method, params) in [("A", ["pad": String(repeating: "x", count: 1 << 20)]), ("B", [:])] {
            group.enter()
            Thread {
                defer { group.leave() }
                let r = (try? c.call(method, params: params)) as? [String: Any]
                lock.withLock { replies[method] = r?["order"] as? [String] }
            }.start()
            waitFor("\(method) was not admitted") { c.admittedMessages.count == (method == "A" ? 1 : 2) }
        }
        waitFor("the writer did not take A, leaving B queued") { c.held.outbound == 1 }
        FileManager.default.createFile(atPath: send, contents: Data())
        XCTAssertEqual(answerAdmitted.wait(timeout: .now() + 5), .success, "srv-1 was not answered")
        // The answer is admitted while B still waits: the child has read nothing, so A cannot have been written.
        XCTAssertEqual(c.held.outbound, 2, "the answer was not queued behind B")
        FileManager.default.createFile(atPath: drain, contents: Data())
        XCTAssertEqual(group.wait(timeout: .now() + 20), .success, "a call did not return")
        XCTAssertEqual(lock.withLock { replies["A"] }, ["A", "B", "srv-1"])
        XCTAssertEqual(lock.withLock { replies["B"] }, ["A", "B", "srv-1"])
    }

    /// After an overflow, the events held before it are delivered once the handler is released, before close; the
    /// event that overflowed and everything after it are not.
    func testEventsHeldAtAnOverflowAreDelivered() throws {
        let maxEvents = 65_536
        let (script, cleanup) = try fakeCodex(
            "call = json.loads(sys.stdin.readline())\n"
                + "sys.stdout.write(''.join(json.dumps({'method': 'e%d' % i}) + '\\n' for i in range(\(maxEvents + 10))))\n"
                + "sys.stdout.write(json.dumps({'id': call['id'], 'result': {}}) + '\\n')\nsys.stdout.flush()\n"
                + "time.sleep(30)\n")
        defer { cleanup() }
        final class Seen: @unchecked Sendable {
            let lock = NSLock()
            var methods: [String] = []
            let release = DispatchSemaphore(value: 0)
            let all = DispatchSemaphore(value: 0)
        }
        let seen = Seen()
        let c = try start(script, timeout: 60) { e in
            let n = seen.lock.withLock { () -> Int in
                seen.methods.append(e.method)
                return seen.methods.count
            }
            if n == 1 { seen.release.wait() }
            if n == maxEvents { seen.all.signal() }
        }
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("faster than the handler"), "\($0)") }
        seen.release.signal()  // before close: the held events drain
        XCTAssertEqual(seen.all.wait(timeout: .now() + 60), .success, "the held events were not delivered")
        c.close()
        // close does not wait for a handler already running: the record is final only once the event thread ends.
        XCTAssertTrue(c.waitForEventLoop(timeout: 5), "the event thread did not end")
        let methods = seen.lock.withLock { seen.methods }
        XCTAssertEqual(methods.count, maxEvents)
        if let i = methods.indices.first(where: { methods[$0] != "e\($0)" }) {
            XCTFail("event \(i) is \(methods[i]), want e\(i)")
        }
    }
}
#endif
