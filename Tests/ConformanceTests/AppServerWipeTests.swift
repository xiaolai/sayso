#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
@testable import RealtimeConnection
import XCTest

/// No byte of a getAuthStatus reply outlives the call but the token the status keeps, as go/codexapp/wipe_test.go
/// checks: the reader's buffer and every buffer it outgrows, the result handed to the call and what is decoded from
/// it are wiped, on success and on every refusal, and a reply that comes after its call stopped waiting is wiped, or
/// never copied (WipeScan.swift). The child is /bin/sh.
final class AppServerWipeTests: XCTestCase {
    override func setUpWithError() throws {
        if MemoryScan.allocatorReplaced { throw XCTSkip("a sanitizer replaced the allocator the scan watches") }
    }

    /// One thing the child sends: a document built in the scan's pages and a newline, after it reads the next request
    /// (`prompted`), or after a moment on its own.
    private struct Answer {
        var parts: [WipeScan.Part]
        var prompted = true
    }

    /// Starts a child that answers initialize, then sends each answer in turn; after the last it reads until its
    /// input ends with its output still open, or exits when `exitAfter` is set. The timeout only catches a child that
    /// hangs: these tests measure memory, and a debug build's work on a 4 MiB line scales with the CPU a loaded machine
    /// leaves it (10 times slower at 10 times oversubscribed), so it is far above the seconds that work takes idle.
    private func start(
        _ w: WipeScan, _ answers: [Answer], timeout: TimeInterval = 300, exitAfter: Bool = false,
        onEvent: (@Sendable (AppServerClient, AppServerEvent) -> Void)? = nil
    ) throws -> AppServerClient {
        let dir = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
            + "/sayso-appwipe-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        addTeardownBlock { try? FileManager.default.removeItem(atPath: dir) }
        var src = "#!/bin/sh\nread -r line\necho '{\"id\":1,\"result\":{\"userAgent\":\"codex_cli_rs/0.162.1\"}}'\nread -r line\n"
        for (i, a) in answers.enumerated() {
            let path = dir + String(format: "/r%02d", i)
            let doc = w.emit(a.parts + [.text("\n")])
            let fd = open(path, O_CREAT | O_WRONLY | O_TRUNC | O_CLOEXEC, 0o600)
            XCTAssertGreaterThanOrEqual(fd, 0)
            XCTAssertEqual(write(fd, doc.baseAddress, doc.count), doc.count)
            close(fd)
            src += (a.prompted ? "read -r line || exit 0\n" : "sleep 0.2\n") + "cat '\(path)'\n"
        }
        src += exitAfter ? "exit 0\n" : "while read -r line; do :; done\n"
        let script = dir + "/codex"
        FileManager.default.createFile(atPath: script, contents: Data(src.utf8))
        chmod(script, 0o755)
        let c = try AppServerClient.start(
            AppServerClient.Configuration(
                bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: timeout, onEvent: onEvent))
        addTeardownBlock { c.close() }
        return c
    }

    /// A reply to the request after the one under test, which proves every line before it was handled.
    private let pong = Answer(parts: [.text(#"{"id":3,"result":{}}"#)])

    private func ping(_ c: AppServerClient, file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertNoThrow(try c.call("ping"), "the client failed before the scan", file: file, line: line)
    }

    func testAnAuthStatusKeepsOnlyItsToken() throws {
        // The third reply is a line of exactly 4 MiB: the head, the token, the padded middle, a needle, the tail.
        let head = #"{"id":2,"result":{"authToken":""#, middle = #"","pad":""#, tail = #"","n":""#
        let pad = String(repeating: "x", count: AppServerClient.maxLine - head.utf8.count - 40 - middle.utf8.count
            - tail.utf8.count - 40 - 3)
        let cases: [(String, [WipeScan.Part], Int)] = [
            ("members it ignores", [.text(#"{"id":2,"result":{"authMethod":"chatgpt","authToken":""#), .kept,
                .text(#"","requiresOpenaiAuth":true,"other":""#), .needle, .text(#"","n":[""#), .escaped,
                .text(#""]},"junk":""#), .needle, .text(#""}"#)], 1 << 16),
            ("an escaped token", [.text(#"{"id":2,"result":{"authToken":""#), .keptEscaped,
                .text(#"","authMethod":"chatgpt"}}"#)], 1 << 16),
            ("a line of 4 MiB", [.text(head), .kept, .text(middle + pad + tail), .needle, .text(#""}}"#)], 5 << 20),
        ]
        for (name, parts, room) in cases {
            let w = WipeScan(room: room)
            let c = try start(w, [Answer(parts: parts), pong])
            var status: AuthStatus?
            let found = w.found {
                do { status = try c.getAuthStatus(includeToken: true, refreshToken: false) } catch {
                    XCTFail("\(name): \(error)")
                }
                ping(c)
            }
            XCTAssertEqual(found, w.want, "\(name): each needle, then each one's first 16 bytes")
            XCTAssertEqual(status?.token.utf8.elementsEqual(w.needle(0)), true, "\(name): not the token sent")
        }
    }

    /// Notifications beside a reply, handed to a handler, leave nothing but the params the handler is given.
    func testANotificationLeavesOnlyItsParams() throws {
        let w = WipeScan()
        let c = try start(w, [
            Answer(parts: [.text(#"{"method":"n","params":{},"x":""#), .needle, .text(#"","y":[""#), .escaped,
                .text("\"]}\n" + #"{"id":2,"result":{"authToken":null}}"#)]), pong,
        ], onEvent: { _, _ in })
        let found = try w.found {
            XCTAssertNoThrow(try c.getAuthStatus(includeToken: false, refreshToken: false))
            ping(c)
        }
        w.check(found)
    }

    func testARefusedAuthStatusLeavesNothing() throws {
        let pad = String(repeating: "x", count: AppServerClient.maxLine)
        let cases: [(String, String, [WipeScan.Part], Int, Bool)] = [
            ("a wrong type after the token", "unexpected getAuthStatus result",
                [.text(#"{"id":2,"result":{"authToken":""#), .needle, .text(#"","requiresOpenaiAuth":"yes"}}"#)], 1 << 16,
                false),
            ("a method that is no string", "unexpected getAuthStatus result",
                [.text(#"{"id":2,"result":{"authToken":""#), .escaped, .text(#"","authMethod":7}}"#)], 1 << 16, false),
            ("a result that is no object", "unexpected getAuthStatus result",
                [.text(#"{"id":2,"result":[""#), .needle, .text(#""]}"#)], 1 << 16, false),
            ("an error naming a registered secret", "rejected getAuthStatus",
                [.text(#"{"id":2,"error":{"code":-32000,"message":"refused "#), .registered, .text(#" there"}}"#)],
                1 << 16, false),
            ("a malformed line", "malformed JSON",
                [.text(#"{"id":2,"result":{"authToken":""#), .needle, .text(#""}"#)], 1 << 16, true),
            // The second needle is 2 MiB in, where only the reader's wipe before the failure clears it.
            ("a line over 4 MiB", "longer than 4 MiB",
                [.text(#"{"id":2,"result":{"authToken":""#), .needle, .text(#"","pad":""# + pad.prefix(2 << 20)),
                    .needle, .text(pad.dropFirst(2 << 20) + #""}}"#)], 9 << 20, true),
        ]
        for (name, want, parts, room, fails) in cases {
            let w = WipeScan(room: room)
            let c = try start(w, [Answer(parts: parts), pong])
            var failure: AppServerError?
            let found = w.found {
                do {
                    _ = try c.getAuthStatus(includeToken: true, refreshToken: false)
                } catch let e as AppServerError {
                    failure = e
                } catch {}
                if !fails { ping(c) }
            }
            XCTAssertEqual(found, w.want, "\(name): each needle, then each one's first 16 bytes")
            XCTAssertTrue(failure?.message.contains(want) ?? false, "\(name): \(String(describing: failure))")
            if failure?.rpcCode != nil { XCTAssertEqual(failure?.rpcMessage, "refused \(Secret.redacted) there", name) }
        }
    }

    /// A reply that comes after its call timed out is never copied and its line is wiped. The child sends it only
    /// once the next request arrives, after a notification that tells it the timed-out request was read; the next
    /// call returning proves the late line was handled. The child holds the reply until then, so the timeout decides
    /// only how long the test waits; it is long enough for a loaded machine to start the child and answer the next call.
    func testALateReplyLeavesNothing() throws {
        let w = WipeScan()
        let c = try start(w, [
            Answer(parts: [.text(#"{"method":"read"}"#)]),
            Answer(parts: [.text(#"{"id":2,"result":{"authToken":""#), .needle, .text("\"}}\n" + #"{"id":3,"result":{}}"#)]),
        ], timeout: 5)
        let found = try w.found {
            XCTAssertThrowsError(try c.getAuthStatus(includeToken: true, refreshToken: false))
            ping(c)
        }
        w.check(found)
    }

    /// After a cancelled call what the child still writes, its late reply included, is read and wiped.
    func testACancelledCallsLateReplyIsDrained() throws {
        let w = WipeScan()
        let c = try start(w, [
            Answer(parts: [.text(#"{"method":"read"}"#)]),
            Answer(parts: [.text(#"{"id":2,"result":{"authToken":""#), .needle, .text(#""}}"#)], prompted: false),
        ], exitAfter: true, onEvent: { client, _ in client.cancel() })
        let found = try w.found {
            XCTAssertThrowsError(try c.getAuthStatus(includeToken: true, refreshToken: false)) {
                XCTAssertEqual(($0 as? AppServerError)?.errorClass, .cancelled)
            }
            XCTAssertTrue(c.waitForReadLoop(timeout: 10), "the reader did not reach the end of the stream")
        }
        w.check(found)
    }

    /// A core whose child runs `script` (its arguments ignored), recording what it admits; no initialize is sent.
    private func core(_ script: String, timeout: TimeInterval) throws -> AppServerCore {
        let path = NSTemporaryDirectory() + "sayso-appwipe-\(UUID().uuidString).sh"
        FileManager.default.createFile(atPath: path, contents: Data("#!/bin/sh\n\(script)\n".utf8))
        chmod(path, 0o755)
        addTeardownBlock { try? FileManager.default.removeItem(atPath: path) }
        let k = try AppServerCore(
            bin: path, env: ["PATH": "/usr/bin:/bin"], timeout: timeout, log: nil, directory: nil, onEvent: nil)
        addTeardownBlock { k.close() }
        k.setTestSeams(recordAdmissions: true)
        return k
    }

    /// Runs `body` on a thread of its own and returns a wait for it.
    private func background(_ body: @escaping @Sendable () -> Void) -> () -> Bool {
        let done = DispatchSemaphore(value: 0)
        Thread {
            body()
            done.signal()
        }.start()
        return { done.wait(timeout: .now() + 20) == .success }
    }

    /// Waits until a call's request is admitted and the writer has taken it: the call is pending.
    private func waitForTheRequest(_ k: AppServerCore) {
        let until = Date() + 10
        while k.admittedMessages.count < 1 || k.held.outbound > 0, Date() < until { usleep(1000) }
    }

    private final class Outcome: @unchecked Sendable {
        private let lock = NSLock()
        private var value: String?
        func set(_ s: String) { lock.withLock { value = s } }
        var get: String? { lock.withLock { value } }
    }

    /// A reply's line is wiped before its call is woken: a call that has its reply leaves no line behind.
    func testAReplysLineIsWipedBeforeItsCallReadsIt() throws {
        let k = try core("while read -r line; do :; done", timeout: 5)
        let line = OwnedBytes(copying: UnsafeRawBufferPointer(#"{"id":1,"result":{"a":1}}"#.utf8.withContiguousStorageIfAvailable { $0 }.map { UnsafeRawBufferPointer($0) }!))
        let outcome = Outcome()
        let done = background {
            do {
                let r = try k.call("m", params: [:]) { result -> String in
                    if line.bytes.contains(where: { $0 != 0 }) { return "the line was not wiped" }
                    return String(decoding: result, as: UTF8.self)
                }
                outcome.set(r)
            } catch { outcome.set("\(error)") }
        }
        waitForTheRequest(k)
        XCTAssertTrue(k.handle(line.bytes))
        XCTAssertTrue(done())
        XCTAssertEqual(outcome.get, #"{"a":1}"#)
    }

    /// A reply copied for a call that then gives up, here while its request is still being written to a child that
    /// never reads, is wiped as the call lets it go.
    func testAReplyCopiedForACallThatGaveUpIsWiped() throws {
        let w = WipeScan()
        let doc = w.emit([.text(#"{"id":1,"result":{"authToken":""#), .needle, .text(#""}}"#)])
        let line = UnsafeMutableRawBufferPointer(mutating: doc)
        let k = try core("exec sleep 30", timeout: 2)
        let outcome = Outcome()
        let found = w.found {
            let done = background {
                do {
                    _ = try k.call("m", params: ["pad": String(repeating: "x", count: 2 << 20)]) { _ in "read" }
                    outcome.set("read")
                } catch { outcome.set("\(error)") }
            }
            waitForTheRequest(k)
            XCTAssertTrue(k.handle(line))
            XCTAssertTrue(done())
        }
        w.check(found)
        XCTAssertTrue(outcome.get?.contains("did not accept a request") ?? false, String(describing: outcome.get))
    }
}
#endif
