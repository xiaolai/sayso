#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import XCTest

/// Event delivery and outbound ordering under failure, close and re-entry, mirroring go/codexapp/lifecycle_test.go.
/// Barriers, not sleeps, order the steps.
final class AppServerLifecycleTests: XCTestCase {
    /// A python child: it answers initialize, reads initialized, then runs `then` (Python source).
    private func fakeCodex(_ then: String) throws -> (String, () -> Void) {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let dir = NSTemporaryDirectory() + "sayso-life-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let script = dir + "/codex"
        let body = "#!\(python)\nimport sys, json, time\nopen(sys.argv[0] + '.ready', 'w').close()\n"
            + "m = json.loads(sys.stdin.readline())\n"
            + "print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n"
            + "sys.stdin.readline()\n" + then
        FileManager.default.createFile(atPath: script, contents: Data(body.utf8))
        chmod(script, 0o755)
        return (script, { try? FileManager.default.removeItem(atPath: dir) })
    }

    /// Records the methods its handler sees; the handler of the first event blocks until `release` is signalled. The
    /// block's own bound is longer than any timeout here: a read of many events slowed by a loaded machine must not
    /// outlast it and let the queue drain before the bounds are reached.
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
                _ = release.waited(600)
            }
        }

        /// Records without blocking.
        func record(_ e: AppServerEvent) { lock.withLock { methods.append(e.method) } }

        var seen: [String] { lock.withLock { methods } }
    }

    /// The timeouts given here only catch a child that hangs, and they include starting Python on a loaded
    /// machine, so they are long: a passing run never waits one out.
    private func start(_ script: String, timeout: TimeInterval, _ onEvent: (@Sendable (AppServerEvent) -> Void)?)
        throws -> AppServerClient
    {
        try AppServerClient.start(
            AppServerClient.Configuration(
                bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: timeout,
                onEvent: onEvent.map { handle in { @Sendable _, e in handle(e) } }))
    }

    private func waitFor(_ what: String, _ cond: () -> Bool) {
        let deadline = Date().addingTimeInterval(5)
        while !cond() {
            if Date() > deadline { return XCTFail("timed out waiting for \(what)") }
            usleep(2000)
        }
    }

    /// A handler that uses the client it is given does not keep the client alive: released without a close, it is
    /// still freed, and its deinit ends the child. As in Go.
    func testHandlerUsingItsClientLetsItBeReleased() throws {
        let (script, cleanup) = try fakeCodex("print(json.dumps({'method': 'hello'}), flush=True)\ntime.sleep(30)\n")
        defer { cleanup() }
        let seen = DispatchSemaphore(value: 0)
        final class PID: @unchecked Sendable {
            let lock = NSLock()
            var value: pid_t = 0
        }
        let given = PID()
        weak var released: AppServerClient?
        var pid: pid_t = 0
        try autoreleasepool {
            let c = try AppServerClient.start(
                AppServerClient.Configuration(
                    bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 60,
                    onEvent: { client, _ in
                        given.lock.withLock { given.value = client.pid }
                        seen.signal()
                    }))
            pid = c.pid
            released = c
            XCTAssertEqual(seen.wait(timeout: .now() + 5), .success, "the event was not delivered")
        }
        XCTAssertEqual(given.lock.withLock { given.value }, pid, "the handler was given another client")
        waitFor("the client to be released") { released == nil }
        waitFor("the child to end") { kill(pid, 0) != 0 }
    }

    /// Releasing a client never blocks the releasing thread on its close, which waits seconds for a child that
    /// ignores SIGTERM; the child still ends.
    func testReleaseDoesNotWaitForTheClose() throws {
        let (script, cleanup) = try fakeCodex(
            "import signal\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
                + "print(json.dumps({'method': 'ignoring'}), flush=True)\ntime.sleep(30)\n")
        defer { cleanup() }
        let ignoring = DispatchSemaphore(value: 0)
        var c: AppServerClient? = try start(script, timeout: 60) { _ in ignoring.signal() }
        XCTAssertEqual(ignoring.wait(timeout: .now() + 5), .success, "the child did not start ignoring SIGTERM")
        let pid = try XCTUnwrap(c).pid
        weak var released: AppServerClient?  // assigned apart: `weak let` needs Swift 6.2, which CI's compiler predates
        released = c
        let start = Date()
        c = nil
        let took = Date().timeIntervalSince(start)
        XCTAssertNil(released, "the client was not released")
        // A release that waited would wait the kill grace at least, for a child that ignores SIGTERM.
        XCTAssertLessThan(took, AppServerClient.killGrace / 2, "the release waited for the close")
        let deadline = Date().addingTimeInterval(AppServerClient.killGrace + 3)
        while kill(pid, 0) == 0, Date() < deadline { usleep(20_000) }
        XCTAssertNotEqual(kill(pid, 0), 0, "the child outlived the client")
    }

    /// Close fails every call with the close, even while an answer to a server request is stuck in a write the child
    /// does not read: the stuck answer does not get to blame the child first. As in Go.
    func testCloseDuringAStuckAnswerReportsTheClose() throws {
        let (script, cleanup) = try fakeCodex(
            "for i in range(5000):\n    sys.stdout.write(json.dumps({'id': 'srv-%d' % i, 'method': 'x'}) + '\\n')\n"
                + "sys.stdout.flush()\ntime.sleep(30)\n")
        defer { cleanup() }
        let c = try start(script, timeout: 60, nil)
        waitFor("the answers to back up") { c.held.refusalBytes > 256 << 10 }
        c.setTestSeams(recordAdmissions: true)
        final class Outcome: @unchecked Sendable {
            let lock = NSLock()
            var error: AppServerError?
        }
        let outcome = Outcome()
        let done = DispatchSemaphore(value: 0)
        Thread.detachNewThread {
            do { _ = try c.call("m") } catch let e as AppServerError { outcome.lock.withLock { outcome.error = e } } catch {}
            done.signal()
        }
        waitFor("the call to be queued") {
            c.admittedMessages.contains { String(decoding: $0, as: UTF8.self).contains(#""method":"m""#) }
        }
        c.close()
        XCTAssertEqual(done.wait(timeout: .now() + 10), .success, "the call did not return")
        XCTAssertEqual(outcome.lock.withLock { outcome.error?.message }, "Codex app-server was closed")
    }

    /// A host that ignores SIGCHLD has the kernel reap the child at exit, so by the time close runs its ID, and its
    /// group ID, may be another process's: close then signals nothing, even when a group with that ID is alive.
    func testCloseSignalsNothingOnceTheSystemReapedTheChild() throws {
        let (script, cleanup) = try fakeCodex("import os\nos._exit(0)\n")
        defer { cleanup() }
        signal(SIGCHLD, SIG_IGN)
        defer { signal(SIGCHLD, SIG_DFL) }
        let c = try start(script, timeout: 60, nil)
        waitFor("the child to exit") { c.hasExited }
        var info = siginfo_t()
        XCTAssertNotEqual(waitid(P_PID, id_t(c.pid), &info, WEXITED | WNOWAIT | WNOHANG), 0, "SIGCHLD was not ignored")
        final class Sent: @unchecked Sendable {
            let lock = NSLock()
            var signals: [Int32] = []
        }
        let sent = Sent()
        c.setProcessSeams(liveGroup: { true }, signal: { sig in sent.lock.withLock { sent.signals.append(sig) } })
        let start = Date()
        c.close()
        XCTAssertEqual(sent.lock.withLock { sent.signals }, [], "close signalled a group that is not ours")
        XCTAssertLessThan(Date().timeIntervalSince(start), AppServerClient.killGrace, "close waited on a group that is not ours")
    }

    /// After an ordinary failure (here malformed output), the events queued before it are still delivered, in order;
    /// nothing read after it is.
    func testEventsQueuedBeforeAFailureAreDelivered() throws {
        let (script, cleanup) = try fakeCodex(
            "sys.stdin.readline()\nfor m in ['a', 'b', 'c']:\n    print(json.dumps({'method': m}), flush=True)\n"
                + "print('not json', flush=True)\nprint(json.dumps({'method': 'after'}), flush=True)\ntime.sleep(30)\n")
        defer { cleanup() }
        let r = Recorder()
        let c = try start(script, timeout: 60) { r.handle($0) }
        defer { c.close() }
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("malformed"), "\($0)") }
        XCTAssertTrue(r.entered.waited(), "no event reached the handler")
        r.release.signal()
        waitFor("the queued events") { r.seen.count == 3 }
        XCTAssertEqual(r.seen, ["a", "b", "c"])
    }

    /// Once close has begun no queued event is admitted, and close does not wait for the handler already running.
    func testCloseAdmitsNoFurtherEvent() throws {
        let (script, cleanup) = try fakeCodex(
            "call = json.loads(sys.stdin.readline())\nfor m in ['first', 'second', 'third']:\n"
                + "    print(json.dumps({'method': m}), flush=True)\n"
                + "print(json.dumps({'id': call['id'], 'result': {}}), flush=True)\ntime.sleep(30)\n")
        defer { cleanup() }
        let r = Recorder()
        let c = try start(script, timeout: 60) { r.handle($0) }
        _ = try c.call("echo")  // its reply follows the three events, so all three are queued
        XCTAssertTrue(r.entered.waited(), "no event reached the handler")
        let closed = DispatchSemaphore(value: 0)
        Thread {
            c.close()
            closed.signal()
        }.start()
        XCTAssertEqual(closed.wait(timeout: .now() + 5), .success, "close waited for the running handler")
        // The barrier is the state close leaves: the queue is released and no event can be queued after close began,
        // so once the running handler returns there is nothing left to admit.
        let held = c.held
        XCTAssertEqual(held.events, 0, "events kept after close")
        XCTAssertEqual(held.eventBytes, 0, "event charge kept after close")
        r.release.signal()
        // The delivery barrier: the event thread ends once the running handler returns, and only then is the record
        // final.
        XCTAssertTrue(c.waitForEventLoop(timeout: 5), "the event thread did not end")
        XCTAssertEqual(r.seen, ["first"])
    }

    /// An event is admitted when the event thread takes it from the queue, under the lock close takes to begin: one
    /// taken before close began is delivered although close runs before its handler is called, and none queued behind
    /// it is.
    func testEventAdmittedBeforeCloseIsDeliveredAndNoneAfter() throws {
        let (script, cleanup) = try fakeCodex(
            "call = json.loads(sys.stdin.readline())\nfor m in ['a', 'b', 'c']:\n"
                + "    print(json.dumps({'method': m}), flush=True)\n"
                + "print(json.dumps({'id': call['id'], 'result': {}}), flush=True)\ntime.sleep(30)\n")
        defer { cleanup() }
        let r = Recorder()
        let admitted = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)
        let once = NSLock()
        nonisolated(unsafe) var first = true
        let c = try start(script, timeout: 60) { e in
            r.record(e)
        }
        // A throw or a failed wait below must not leave the event thread blocked in the seam; an extra signal is
        // harmless.
        defer {
            release.signal()
            c.close()
        }
        c.setTestSeams(eventAdmitted: {
            let isFirst = once.withLock { () -> Bool in
                defer { first = false }
                return first
            }
            if isFirst {
                admitted.signal()
                _ = release.waited()
            }
        })
        _ = try c.call("echo")  // its reply follows the three events
        XCTAssertEqual(admitted.wait(timeout: .now() + 5), .success, "no event was admitted")
        // "a" is taken; its handler has not been called. close runs on a worker so a close that waited for the
        // handler fails the bounded wait instead of deadlocking the test.
        let closed = DispatchSemaphore(value: 0)
        Thread {
            c.close()
            closed.signal()
        }.start()
        XCTAssertEqual(closed.wait(timeout: .now() + 5), .success, "close waited for the admitted event's handler")
        release.signal()
        XCTAssertTrue(c.waitForEventLoop(timeout: 5), "the event thread did not end")
        XCTAssertEqual(r.seen, ["a"])
        XCTAssertEqual(c.held.events + c.held.eventBytes, 0)
    }

    /// A handler may call `call` and `close` itself: neither waits for the event thread.
    func testHandlerMayCallAndClose() throws {
        let (script, cleanup) = try fakeCodex(
            "print(json.dumps({'method': 'go'}), flush=True)\nfor line in sys.stdin:\n    m = json.loads(line)\n"
                + "    if 'id' in m and 'method' in m:\n"
                + "        print(json.dumps({'id': m['id'], 'result': {'ok': True}}), flush=True)\n")
        defer { cleanup() }
        final class Box: @unchecked Sendable { var client: AppServerClient? }
        let box = Box()
        let ready = DispatchSemaphore(value: 0)
        let done = DispatchSemaphore(value: 0)
        let ok = Box()
        let c = try start(script, timeout: 60) { _ in
            _ = ready.waited()
            if let client = box.client, let r = try? client.call("echo"), (r as? [String: Any])?["ok"] as? Bool == true {
                ok.client = client
            }
            box.client?.close()
            done.signal()
        }
        box.client = c
        ready.signal()
        XCTAssertEqual(done.wait(timeout: .now() + 10), .success, "call or close from the handler deadlocked")
        XCTAssertNotNil(ok.client, "the call from the handler failed")
    }

    /// Events beyond the count bound fail the client with the overflow, even when each is empty, and a reply the
    /// child writes after them is not handed out. Later calls fail at once. The timeout leaves a debug build on a
    /// loaded machine time to read 70,000 lines (over 60 s at 30 times oversubscribed): the bound, not the speed, is
    /// under test.
    func testEmptyEventsTripTheBounds() throws {
        let (script, cleanup) = try fakeCodex(
            "call = json.loads(sys.stdin.readline())\nsys.stdout.write((json.dumps({'method': ''}) + '\\n') * 70000)\n"
                + "sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"
                + "sys.stdout.flush()\ntime.sleep(30)\n")
        defer { cleanup() }
        let r = Recorder()
        let c = try start(script, timeout: 300) { r.handle($0) }
        defer {
            c.close()
            r.release.signal()
        }
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("faster than the handler"), "\($0)") }
        let t0 = Date()
        XCTAssertThrowsError(try c.call("echo")) { XCTAssertTrue("\($0)".contains("faster than the handler"), "\($0)") }
        // A later call that did not fail at once would wait out the timeout; the bound is half of it.
        XCTAssertLessThan(Date().timeIntervalSince(t0), 150, "a later call did not fail at once")
    }

    /// The bounds charge the event being handled too, so the boundary is exact: 65,536 events are held, one more
    /// fails the client. The timeout is long for the reason the test above gives.
    func testEventCountBoundIsExact() throws {
        for (n, fails) in [(65_536, false), (65_537, true)] {
            let (script, cleanup) = try fakeCodex(
                "call = json.loads(sys.stdin.readline())\nsys.stdout.write((json.dumps({'method': ''}) + '\\n') * \(n))\n"
                    + "sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"
                    + "sys.stdout.flush()\ntime.sleep(30)\n")
            defer { cleanup() }
            let r = Recorder()
            let c = try start(script, timeout: 300) { r.handle($0) }
            let failed = (try? c.call("echo")) == nil
            XCTAssertEqual(failed, fails, "\(n) events")
            c.close()
            r.release.signal()
        }
    }

    /// An answer to a server request that cannot be written fails the client: the child would wait for it, and every
    /// later request would be written ahead of it.
    func testUnwritableRefusalFailsTheClient() throws {
        // The child never reads again: its input pipe fills with refusals, and the next one cannot be written at all.
        let (script, cleanup) = try fakeCodex(
            "for i in range(3000):\n"
                + "    sys.stdout.write(json.dumps({'id': 'srv-%d' % i, 'method': 'approve', 'params': {'pad': 'x' * 100}}) + '\\n')\n"
                + "sys.stdout.flush()\ntime.sleep(30)\n")
        defer { cleanup() }
        let c = try AppServerClient.startWhenReady(.init(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3))
        defer { c.close() }
        var last = ""
        let deadline = Date().addingTimeInterval(20)
        while Date() < deadline {
            do { _ = try c.call("echo") } catch { last = "\(error)" }
            if last.contains("did not read the answer to a server request") || last.contains("cut off") { break }
        }
        XCTAssertTrue(
            last.contains("did not read the answer to a server request") || last.contains("cut off"), "failure: \(last)")
        // Failing at once repeats the failure; a call that was written would wait out the timeout and name that, so the
        // bound is half the timeout.
        let t0 = Date()
        XCTAssertThrowsError(try c.call("echo")) {
            XCTAssertTrue("\($0)".contains("did not read the answer to a server request") || "\($0)".contains("cut off"), "\($0)")
        }
        XCTAssertLessThan(Date().timeIntervalSince(t0), 1.5, "a call after the failure did not fail at once")
    }
}

extension AppServerClient {
    /// Starts a client whose handshake begins once the child has created `<bin>.ready`, as a fake does when its
    /// interpreter is up: a short timeout under test then times the exchange, not starting Python on a loaded machine.
    static func startWhenReady(_ cfg: Configuration) throws -> AppServerClient {
        try start(cfg) { _ in
            let deadline = Date().addingTimeInterval(60)
            while access(cfg.bin + ".ready", F_OK) != 0, Date() < deadline { usleep(2000) }
        }
    }
}
#endif
