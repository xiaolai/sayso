import XCTest

@testable import RealtimeConnection

/// Deadlines, cancelled sends and close after a failure.
final class SessionLifecycleTests: XCTestCase {
    func testNonPositiveDeadlineExpiresBeforeTheOperation() async throws {
        for seconds in [0.0, -1.0] {
            let closes = CloseCounter()
            let ran = FakeOpenAI.Flag()
            do {
                _ = try await RealtimeDeadline.run(seconds: seconds, close: { closes.add() }) {
                    ran.set()
                    return 1
                }
                XCTFail("\(seconds): the deadline did not expire")
            } catch is RealtimeDeadline.Expired {}
            XCTAssertEqual(closes.value, 1, "\(seconds)")
            XCTAssertFalse(ran.value, "\(seconds): the operation ran")
        }
    }

    func testOperationFinishingAfterTheInstantIsExpired() async throws {
        let closes = CloseCounter()
        do {
            _ = try await RealtimeDeadline.run(seconds: 0.01, close: { closes.add() }) {
                // Busy past the deadline without suspending, so the timer cannot run first.
                let end = Date().addingTimeInterval(0.2)
                while Date() < end {}
                return 1
            }
            XCTFail("an overdue operation succeeded")
        } catch is RealtimeDeadline.Expired {}
        XCTAssertEqual(closes.value, 1)
    }

    private func open(_ server: FakeWebSocketServer) async throws -> RealtimeSession {
        let client = try FakeOpenAI.client(port: server.port)
        return try await RealtimeSession.open(
            client: client, secret: ClientSecret(bearer: "ek_lifecycle", expiresAt: Date()),
            configuration: RealtimeSession.Configuration(
                url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
                handshakeTimeout: 5, readLimit: 1 << 20))
    }

    func testCancelledSendSendsNothing() async throws {
        let server = try FakeWebSocketServer(message: Data(#"{"type":"input_audio_buffer.committed","item_id":"i"}"#.utf8))
        defer { server.stop() }
        let session = try await open(server)
        let sender = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            try await session.send(Data("{}".utf8))
        }
        do {
            try await sender.value
            XCTFail("a cancelled send succeeded")
        } catch is CancellationError {}
        // The server cancels the connection on the first message it receives; none arrived, so the session lives.
        let m = try await session.next()
        XCTAssertEqual(m.event, .inputCommitted(itemID: "i"))
        await session.close()
    }

    func testNextAfterFailureThenCloseIsClosed() async throws {
        let server = try FakeWebSocketServer(message: Data(#"{"type":"input_audio_buffer.committed","item_id":"i"}"#.utf8))
        defer { server.stop() }
        let session = try await open(server)
        _ = try await session.next()
        try await session.send(Data("{}".utf8))  // the server drops the connection on receipt
        do {
            _ = try await RealtimeDeadline.run(seconds: 10, close: { Task { await session.close() } }) {
                try await session.next()
            }
            XCTFail("next succeeded on a dropped connection")
        } catch is RealtimeError {}
        await session.close()
        do {
            _ = try await session.next()
            XCTFail("next after close")
        } catch is RealtimeSession.Closed {}
    }
}

final class CloseCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    func add() { lock.withLock { n += 1 } }
    var value: Int { lock.withLock { n } }
}
