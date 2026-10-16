import Foundation
import XCTest

@testable import RealtimeConnection

/// The session below the fixtures: its write path, its unknown-type accounting, and opening.
final class SessionWriteTests: XCTestCase {
    private final class Log: @unchecked Sendable {
        private let lock = NSLock()
        private var entries: [String] = []
        func add(_ s: String) { lock.withLock { entries.append(s) } }
        var values: [String] { lock.withLock { entries } }
    }

    private func waitUntil(_ condition: () -> Bool, file: StaticString = #filePath, line: UInt = #line) async throws {
        let deadline = Date().addingTimeInterval(5)
        while !condition(), Date() < deadline { try await Task.sleep(nanoseconds: 2_000_000) }
        XCTAssertTrue(condition(), "the condition never held", file: file, line: line)
    }

    // MARK: Admission

    func testAdmissionSettlesCancellationAgainstTheStart() {
        let first = WriteAdmission()
        XCTAssertFalse(first.cancel(), "nothing in flight to end")
        XCTAssertFalse(first.start(), "a write started after its cancellation")

        let inFlight = WriteAdmission()
        XCTAssertTrue(inFlight.start())
        XCTAssertTrue(inFlight.cancel(), "a write in flight is ended")

        let done = WriteAdmission()
        XCTAssertTrue(done.start())
        done.finish()
        XCTAssertFalse(done.cancel(), "a finished write's transport is left alone")
    }

    /// A task cancelled before the handler is installed, the window between a cancellation check and the write,
    /// neither writes nor ends the transport.
    func testCancelledBeforeTheWriteTouchesNothing() async {
        let log = Log()
        let task = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            try await RealtimeSession.admitted { log.add("write") } end: { log.add("end") }
        }
        do {
            try await task.value
            XCTFail("a cancelled write ran")
        } catch is CancellationError {} catch { XCTFail("\(error)") }
        XCTAssertEqual(log.values, [])
    }

    func testCancellingAWriteInFlightEndsIt() async throws {
        let log = Log()
        let task = Task {
            try await RealtimeSession.admitted {
                log.add("write")
                try? await Task.sleep(nanoseconds: 5_000_000_000)
            } end: { log.add("end") }
        }
        try await waitUntil { log.values == ["write"] }
        task.cancel()
        _ = try? await task.value
        XCTAssertEqual(log.values, ["write", "end"])
    }

    // MARK: The write queue

    func testCloseReleasesEveryQueuedWriter() async throws {
        let gate = WriteGate()
        try await gate.acquire()
        let outcomes = Log()
        let waiters = (0..<3).map { _ in
            Task {
                do {
                    try await gate.acquire()
                    outcomes.add("turn")
                } catch is RealtimeSession.Closed { outcomes.add("closed") } catch { outcomes.add("\(error)") }
            }
        }
        try await waitUntil { gate.waiting == 3 }
        gate.close()
        try await waitUntil { outcomes.values.count == 3 }  // bounded: a writer left queued would hang the test
        XCTAssertEqual(outcomes.values, ["closed", "closed", "closed"])
        for w in waiters { w.cancel() }
        let late = Task {
            do {
                try await gate.acquire()
                outcomes.add("late turn")
            } catch is RealtimeSession.Closed { outcomes.add("late closed") } catch { outcomes.add("late \(error)") }
        }
        try await waitUntil { outcomes.values.count == 4 }
        XCTAssertEqual(outcomes.values.last, "late closed", "a writer got the turn, or queued, after close")
        late.cancel()
        gate.release()
    }

    func testCancelledWaitersAreSkippedInOrder() async throws {
        let gate = WriteGate()
        try await gate.acquire()
        let order = Log()
        var tasks: [Task<Void, Error>] = []
        for name in ["a", "b", "c", "d"] {
            tasks.append(
                Task {
                    try await gate.acquire()
                    order.add(name)
                    gate.release()
                })
            try await waitUntil { gate.waiting == tasks.count }
        }
        tasks[1].cancel()
        tasks[2].cancel()
        try await waitUntil { gate.waiting == 2 }
        gate.release()
        try await waitUntil { order.values.count == 2 }  // bounded: a waiter left behind a cancelled one would hang
        XCTAssertEqual(order.values, ["a", "d"])
        for t in tasks { t.cancel() }
        try await gate.acquire()  // free again: the cancelled ids left nothing holding the turn
        gate.release()
    }

    // MARK: Strict UTF-8

    func testInvalidUTF8IsRefusedAndTheSessionStaysOpen() async throws {
        let server = try FakeWebSocketServer(message: Data(#"{"type":"session.created"}"#.utf8))
        defer { server.stop() }
        let session = try await RealtimeSession.open(
            client: FakeOpenAI.client(port: server.port),
            secret: ClientSecret(bearer: "ek_utf8", expiresAt: Date()),
            configuration: .init(
                url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
                handshakeTimeout: 5, readLimit: 1 << 20))
        for bad in [[0x7B, 0x22, 0xFF, 0x22, 0x7D], [0x7B, 0x22, 0xED, 0xA0, 0x80, 0x22, 0x7D], [0xC0, 0xAF]] as [[UInt8]] {
            do {
                try await session.send(Data(bad))
                XCTFail("\(bad): invalid UTF-8 was sent")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .internal)
            }
        }
        try await session.send(Data(#"{"type":"input_audio_buffer.clear","é":"ok"}"#.utf8))
        await session.close()
    }

    // MARK: Checking before the secret

    private final class CountingSource: SecretSource, @unchecked Sendable {
        private let lock = NSLock()
        private var n = 0
        var calls: Int { lock.withLock { n } }
        func secret(session: Data) async throws -> ClientSecret {
            lock.withLock { n += 1 }
            return ClientSecret(bearer: "ek_counted", expiresAt: Date())
        }
    }

    func testConfigurationAndDestinationAreCheckedBeforeTheSecret() async throws {
        let source = CountingSource()
        let client = try FakeOpenAI.client(port: 9)
        let local = RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:9")!)
        let cases: [(String, RealtimeSession.Configuration)] = [
            ("zero read limit", .init(url: local, handshakeTimeout: 5, readLimit: 0)),
            ("infinite timeout", .init(url: local, handshakeTimeout: .infinity, readLimit: 1 << 20)),
            ("forbidden origin", .init(
                url: RealtimeSession.url(model: "m", base: URL(string: "wss://example.invalid")!), handshakeTimeout: 5,
                readLimit: 1 << 20)),
        ]
        for (name, configuration) in cases {
            do {
                _ = try await RealtimeSession.open(
                    client: client, source: source, session: Data("{}".utf8), configuration: configuration)
                XCTFail("\(name): opened")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .internal, name)
            }
        }
        XCTAssertEqual(source.calls, 0, "a secret was fetched for a configuration that cannot open")
    }
}
