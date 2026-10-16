import Foundation
import Network
import XCTest

@testable import RealtimeConnection

/// RealtimeDeadline, session close and cancellation, and the HTTP client's limits, beyond the fixtures.
final class SessionDeadlineTests: XCTestCase {
    final class Counter: @unchecked Sendable {
        private let lock = NSLock()
        private var n = 0
        func add() { lock.withLock { n += 1 } }
        var value: Int { lock.withLock { n } }
    }

    func testCancelledBeforeEntryClosesOnce() async {
        let closes = Counter()
        let task = Task { () -> Int in
            withUnsafeCurrentTask { $0?.cancel() }
            return try await RealtimeDeadline.run(seconds: 5, close: { closes.add() }) { 1 }
        }
        do {
            _ = try await task.value
            XCTFail("a cancelled run returned")
        } catch is CancellationError {} catch { XCTFail("\(error)") }
        XCTAssertEqual(closes.value, 1)
    }

    func testCancelledDuringTheOperationClosesOnce() async {
        let closes = Counter()
        let closed = FakeOpenAI.Flag()
        let started = FakeOpenAI.Flag()
        let task = Task { () -> Int in
            try await RealtimeDeadline.run(seconds: 30, close: { closes.add(); closed.set() }) {
                started.set()
                while !closed.value { try? await Task.sleep(nanoseconds: 2_000_000) }  // a receive close unblocks
                return 1
            }
        }
        while !started.value { try? await Task.sleep(nanoseconds: 2_000_000) }
        task.cancel()
        do {
            _ = try await task.value
            XCTFail("a cancelled run returned")
        } catch is CancellationError {} catch { XCTFail("\(error)") }
        XCTAssertEqual(closes.value, 1)
    }

    func testCancellationRacingExpiryClosesOnce() async {
        for _ in 0..<50 {
            let closes = Counter()
            let closed = FakeOpenAI.Flag()
            let task = Task { () -> Int in
                try await RealtimeDeadline.run(seconds: 0.005, close: { closes.add(); closed.set() }) {
                    while !closed.value { try? await Task.sleep(nanoseconds: 1_000_000) }
                    return 1
                }
            }
            try? await Task.sleep(nanoseconds: 5_000_000)
            task.cancel()
            _ = try? await task.value
            XCTAssertEqual(closes.value, 1)
        }
    }

    func testRunReturnsOnlyAfterCloseFinished() async {
        let closed = FakeOpenAI.Flag()
        let finished = FakeOpenAI.Flag()
        do {
            _ = try await RealtimeDeadline.run(
                seconds: 0.02,
                close: {
                    closed.set()
                    Thread.sleep(forTimeInterval: 0.2)  // a slow transport shutdown
                    finished.set()
                }
            ) { () async throws -> Int in
                while !closed.value { try await Task.sleep(nanoseconds: 1_000_000) }
                return 1
            }
            XCTFail("the deadline did not expire")
        } catch is RealtimeDeadline.Expired {} catch { XCTFail("\(error)") }
        XCTAssertTrue(finished.value, "close was still running after run returned")
    }

    func testUnrepresentableDeadlinesAreRefused() async {
        for seconds in [TimeInterval.infinity, .nan, 1e300] {
            do {
                _ = try await RealtimeDeadline.run(seconds: seconds, close: {}) { 1 }
                XCTFail("\(seconds) accepted")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .internal)
            } catch { XCTFail("\(error)") }
        }
        do {
            _ = try await RealtimeDeadline.run(seconds: 0, close: {}) { 1 }
        } catch is RealtimeDeadline.Expired {} catch { XCTFail("zero must expire at once, not be refused: \(error)") }
    }

    func testUnrepresentableTimeoutsAreRefused() async throws {
        let client = HTTPClient()
        for timeout in [TimeInterval.infinity, .nan, 1e300, 0, -1] {
            do {
                _ = try await client.send(URLRequest(url: URL(string: "https://api.openai.com/")!), timeout: timeout, limit: 1)
                XCTFail("send accepted \(timeout)")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .internal)
            }
            do {
                _ = try await Minter(client: client, credentials: NoCredentials(), timeout: timeout, lifetimeSeconds: 60)
                    .secret(session: Data("{}".utf8))
                XCTFail("Minter accepted \(timeout)")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .internal)
            }
            do {
                _ = try await RealtimeSession.open(
                    client: client, secret: ClientSecret(bearer: "ek_t", expiresAt: Date()),
                    configuration: .init(url: RealtimeSession.url(model: "m"), handshakeTimeout: timeout, readLimit: 10))
                XCTFail("open accepted \(timeout)")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .internal)
            }
        }
        do {
            _ = try await RealtimeSession.open(
                client: client, secret: ClientSecret(bearer: "ek_t", expiresAt: Date()),
                configuration: .init(url: RealtimeSession.url(model: "m"), handshakeTimeout: 5, readLimit: .max))
            XCTFail("open accepted readLimit Int.max")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .internal)
        }
    }

    struct NoCredentials: CredentialSource {
        func token() async throws -> Token { throw RealtimeError(.internal, message: "not reached") }
        func refresh(after stale: Token) async throws -> Token { throw RealtimeError(.internal, message: "not reached") }
    }

    func testConfigurationCannotSmuggleABearerOrHost() async throws {
        let server = try FakeHTTPServer(replies: [FakeHTTPReply(status: 200, headers: [], body: Data("ok".utf8))])
        let port = try server.start()
        defer { server.stop() }
        let config = URLSessionConfiguration.ephemeral
        config.httpAdditionalHeaders = ["AUTHORIZATION": "Bearer ek_smuggled", "Host": "api.openai.com", "X-Kept": "1"]
        let client = HTTPClient(policy: try OriginPolicy(testOrigins: ["127.0.0.1:\(port)"]), configuration: config)
        _ = try await client.send(URLRequest(url: URL(string: "http://127.0.0.1:\(port)/")!), timeout: 5, limit: 10)
        let headers = try XCTUnwrap(server.requests.first?.headers)
        XCTAssertNil(headers["authorization"])
        XCTAssertNotEqual(headers["host"], "api.openai.com")
        XCTAssertEqual(headers["x-kept"], "1")
    }

    func testBracketedIPv6TestOrigin() throws {
        let policy = try OriginPolicy(testOrigins: ["[::1]:8080"])
        var request = URLRequest(url: URL(string: "http://[::1]:8080/v1")!)
        request.setValue("Bearer ek_x", forHTTPHeaderField: "Authorization")
        XCTAssertNoThrow(try policy.check(request))
        let unbracketed = try OriginPolicy(testOrigins: ["::1:8080"])
        XCTAssertNoThrow(try unbracketed.check(request))
    }

    /// A 401 whose body is cut short, or stalls past the deadline, is still auth-expired, as in Go.
    func testErrorBodyFailureKeepsTheStatus() async throws {
        for hold in [false, true] {
            let server = try RawServer(head: "HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"error\":", hold: hold)
            defer { server.stop() }
            let client = HTTPClient(policy: try OriginPolicy(testOrigins: ["127.0.0.1:\(server.port)"]))
            do {
                _ = try await client.send(URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)/")!), timeout: 1, limit: 10)
                XCTFail("a 401 succeeded")
            } catch let e as RealtimeError {
                XCTAssertEqual(e.errorClass, .authExpired, "hold \(hold): \(e)")
                XCTAssertEqual(e.status, 401)
            }
        }
    }

    /// A server that answers every connection with `head` and then either closes (a truncated body) or holds the
    /// connection open (a stalled one).
    final class RawServer: @unchecked Sendable {
        final class Held: @unchecked Sendable {
            let lock = NSLock()
            var connections: [NWConnection] = []
        }
        private let listener: NWListener
        private let queue = DispatchQueue(label: "raw-server")
        private let held = Held()
        let port: UInt16

        init(head: String, hold: Bool) throws {
            listener = try NWListener(using: FakeOpenAI.loopbackParameters(NWParameters(tls: nil, tcp: .init())))
            let queue = self.queue
            let held = self.held
            listener.newConnectionHandler = { connection in
                connection.start(queue: queue)
                connection.receive(minimumIncompleteLength: 1, maximumLength: 65536) { _, _, _, _ in
                    connection.send(
                        content: Data(head.utf8),
                        completion: .contentProcessed { _ in
                            if hold { held.lock.withLock { held.connections.append(connection) } } else { connection.cancel() }
                        })
                }
            }
            port = try FakeOpenAI.waitReady(listener, queue: queue)
        }

        func stop() {
            listener.cancel()
            held.lock.withLock { held.connections.forEach { $0.cancel() } }
        }
    }

    func testCloseWithABufferedMessageEndsEveryWait() async throws {
        let raw = Data(#"{"type":"input_audio_buffer.committed","item_id":"i"}"#.utf8)
        let server = try FakeWebSocketServer(message: raw)
        defer { server.stop() }
        let client = try FakeOpenAI.client(port: server.port)
        let session = try await RealtimeSession.open(
            client: client, secret: ClientSecret(bearer: "ek_buffered", expiresAt: Date()),
            configuration: .init(
                url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
                handshakeTimeout: 5, readLimit: 1 << 20))
        let buffered = await eventually { session.holdsBufferedMessage }
        XCTAssertTrue(buffered, "the message never arrived")
        await session.close()
        for _ in 0..<2 {
            do {
                _ = try await RealtimeDeadline.run(seconds: 5, close: {}) { try await session.next() }
                XCTFail("a message was delivered after close")
            } catch is RealtimeSession.Closed {} catch { XCTFail("\(error)") }
        }
    }
}
