import Foundation
import Network
import XCTest

@testable import RealtimeConnection

/// Round-3 regressions: deadline bounds, write serialisation, close, counting, quoting, origins and close frames.
final class SessionCloseTests: XCTestCase {
    // MARK: Deadlines

    func testDeadlineBeyondInt64NanosecondsIsRefusedNotTrapped() async throws {
        do {
            _ = try await RealtimeDeadline.run(seconds: 10_000_000_000, close: {}) { 1 }
            XCTFail("an unrepresentable deadline was accepted")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .internal)
        }
        let value = try await RealtimeDeadline.run(seconds: 9_000_000_000, close: {}) { 7 }
        XCTAssertEqual(value, 7)
        XCTAssertNil(Deadline.nanoseconds(9_300_000_000))
    }

    func testRequestDeadlineRejectsALateSuccess() async {
        do {
            _ = try await withDeadline(seconds: 0.01, what: "request") { () async throws -> Int in
                let end = Date().addingTimeInterval(0.2)  // busy past the instant without suspending
                while Date() < end {}
                return 1
            }
            XCTFail("a late success was admitted")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .network)
        } catch {
            XCTFail("\(error)")
        }
    }

    func testCallerCancellationIsCancelledNotAnHTTPRefusal() async {
        let task = Task { () async throws -> Int in
            try await withDeadline(seconds: 30, what: "request") { () async throws -> Int in
                // An error body cut short by the cancellation, still classified by its status.
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                throw RealtimeError(.authExpired, status: 401)
            }
        }
        try? await Task.sleep(nanoseconds: 50_000_000)
        task.cancel()
        do {
            _ = try await task.value
            XCTFail("a cancelled request succeeded")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .cancelled)
        } catch {
            XCTFail("\(error)")
        }
    }

    // MARK: Writes

    func testCancelledWaiterLeavesTheWriteQueue() async throws {
        let gate = WriteGate()
        try await gate.acquire()
        let waiter = Task { try await gate.acquire() }
        try? await Task.sleep(nanoseconds: 50_000_000)
        waiter.cancel()
        do {
            try await waiter.value
            XCTFail("a cancelled waiter got the turn")
        } catch is CancellationError {}
        gate.release()
        let next = Task { try await gate.acquire() }
        let got = FakeOpenAI.blocking { try await next.value }  // returns at once: the cancelled waiter left the queue
        XCTAssertNoThrow(try got.get())
        gate.release()
    }

    func testWritesAreServedInOrder() async throws {
        let gate = WriteGate()
        try await gate.acquire()
        let order = OrderBox()
        let first = Task {
            try await gate.acquire()
            order.add(1)
            gate.release()
        }
        try? await Task.sleep(nanoseconds: 30_000_000)
        let second = Task {
            try await gate.acquire()
            order.add(2)
            gate.release()
        }
        try? await Task.sleep(nanoseconds: 30_000_000)
        gate.release()
        try await first.value
        try await second.value
        XCTAssertEqual(order.values, [1, 2])
    }

    // MARK: Counting and quoting

    func testUnknownTypesAreCountedByTheirBytes() {
        let inbox = Inbox()
        inbox.countUnknown("type.\u{00E9}")
        inbox.countUnknown("type.e\u{0301}")
        inbox.countUnknown("type.\u{00E9}")
        let counts = inbox.unknownCounts
        XCTAssertEqual(counts.count, 2)
        XCTAssertEqual(counts.map(\.count), [2, 1])
        XCTAssertTrue(counts[1].type.utf8.elementsEqual("type.e\u{0301}".utf8))
    }

    func testRateLimitNameIsQuotedAsGoQuotesIt() {
        let e = RealtimeEvent.exhausted([RateLimit(name: "a\nb\"c\\\u{7F}\u{200B}é", limit: 1, remaining: 0, resetSeconds: 1)])
        XCTAssertEqual(e?.message, #"rate limit "a\nb\"c\\\x7f\u200bé" exhausted"#)
    }

    func testMissingTypeIsReportedAsGoReportsIt() {
        func message(_ raw: String) -> String {
            do {
                _ = try RealtimeEvent.decode(Data(raw.utf8))
                return "decoded"
            } catch let e as RealtimeError { return e.message } catch { return "\(error)" }
        }
        XCTAssertEqual(message(#"{"x":1}"#), "realtime event: missing type")
        XCTAssertEqual(message(#"{"type":null}"#), "realtime event: missing type")
        XCTAssertEqual(message(#"{"type":""}"#), "realtime event: missing type")
        XCTAssertEqual(message(#"{"type":5}"#), "realtime event: not valid JSON")
        XCTAssertEqual(message(#"[1]"#), "realtime event: not valid JSON")
    }

    // MARK: Origins and credentials

    func testTestOriginPortIsCanonical() throws {
        let policy = try OriginPolicy(testOrigins: ["127.0.0.1:08080"])
        var request = URLRequest(url: URL(string: "http://127.0.0.1:8080/v1/realtime/client_secrets")!)
        request.setValue("Bearer x", forHTTPHeaderField: "Authorization")
        XCTAssertNoThrow(try policy.check(request))
        for bad in ["127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:+80", "127.0.0.1:８０"] {
            XCTAssertThrowsError(try OriginPolicy(testOrigins: [bad]), bad)
        }
    }

    func testNoStoredOrChallengedCredential() {
        XCTAssertNil(HTTPClient().session.configuration.urlCredentialStorage)
        let basic = URLProtectionSpace(
            host: "api.openai.com", port: 443, protocol: "https", realm: nil,
            authenticationMethod: NSURLAuthenticationMethodHTTPBasic)
        let challenge = URLAuthenticationChallenge(
            protectionSpace: basic, proposedCredential: URLCredential(user: "u", password: "p", persistence: .none),
            previousFailureCount: 0, failureResponse: nil, error: nil, sender: NoSender())
        let disposition = DispositionBox()
        answerChallenge(challenge) { d, credential in disposition.set(d, credential) }
        XCTAssertEqual(disposition.value?.0, .rejectProtectionSpace)
        XCTAssertNil(disposition.value?.1)
    }

    // MARK: Close frames and close

    func testReceivedCloseFramesAreClassifiedOnTheProductionPath() async throws {
        Secret.register("ek_close_reason_canary")
        for (code, want) in [(1008, ErrorClass.invalidRequest), (1011, .server), (1000, .protocol), (4000, .server)] {
            let server = try FakeClosingServer(code: UInt16(code), reason: "bye ek_close_reason_canary")
            defer { server.stop() }
            let session = try await open(port: server.port)
            for attempt in 0..<2 {  // the failure ends the session, and every later call gets it again
                do {
                    _ = try await session.next()
                    XCTFail("\(code): next succeeded")
                } catch let e as RealtimeError {
                    XCTAssertEqual(e.errorClass, want, "\(code) attempt \(attempt)")
                    XCTAssertEqual(e.code, String(code), "\(code) attempt \(attempt)")
                    XCTAssertFalse(e.description.contains("canary"), "\(code): the reason was not scrubbed")
                }
            }
            await session.close()
        }
    }

    func testCloseInACancelledTaskWaitsWithoutSpinning() async throws {
        let server = try FakeClosingServer(code: nil, reason: "")  // never answers the close
        defer { server.stop() }
        let session = try await open(port: server.port)
        let before = cpuSeconds()
        let started = Date()
        let closing = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            await session.close()
        }
        await closing.value
        let wall = Date().timeIntervalSince(started)
        let cpu = cpuSeconds() - before
        XCTAssertLessThan(wall, 5, "close outlived its bound")
        XCTAssertLessThan(cpu, max(0.5, wall / 2), "close spun for \(cpu) s of CPU in \(wall) s")
        do {
            _ = try await session.next()
            XCTFail("next after close")
        } catch is RealtimeSession.Closed {}
    }

    private func open(port: UInt16) async throws -> RealtimeSession {
        let client = try FakeOpenAI.client(port: port)
        return try await RealtimeSession.open(
            client: client, secret: ClientSecret(bearer: "ek_round_three", expiresAt: Date()),
            configuration: RealtimeSession.Configuration(
                url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(port)")!),
                handshakeTimeout: 5, readLimit: 1 << 20))
    }

    private func cpuSeconds() -> Double {
        var usage = rusage()
        getrusage(RUSAGE_SELF, &usage)
        return Double(usage.ru_utime.tv_sec) + Double(usage.ru_utime.tv_usec) / 1e6 + Double(usage.ru_stime.tv_sec)
            + Double(usage.ru_stime.tv_usec) / 1e6
    }
}

/// A WebSocket server that sends one close frame with `code` and `reason` when a client connects, or, with no code,
/// reads nothing, so a close is never answered.
private final class FakeClosingServer: @unchecked Sendable {
    private let listener: NWListener
    private let queue = DispatchQueue(label: "fake-closing-ws")
    let port: UInt16

    init(code: UInt16?, reason: String) throws {
        let parameters = FakeOpenAI.loopbackParameters(NWParameters(tls: nil, tcp: .init()))
        let options = NWProtocolWebSocket.Options()
        options.autoReplyPing = true
        parameters.defaultProtocolStack.applicationProtocols.insert(options, at: 0)
        listener = try NWListener(using: parameters)
        let queue = self.queue
        listener.newConnectionHandler = { connection in
            connection.stateUpdateHandler = { state in
                guard case .ready = state else { return }
                if let code {
                    let metadata = NWProtocolWebSocket.Metadata(opcode: .close)
                    if code >= 3000 {
                        metadata.closeCode = .applicationCode(code)
                    } else if let defined = NWProtocolWebSocket.CloseCode.Defined(rawValue: code) {
                        metadata.closeCode = .protocolCode(defined)
                    }
                    let context = NWConnection.ContentContext(identifier: "close", metadata: [metadata])
                    connection.send(
                        content: Data(reason.utf8), contentContext: context, isComplete: true, completion: .idempotent)
                }  // with no code, nothing is read, so a close frame is never answered
            }
            connection.start(queue: queue)
        }
        port = try FakeOpenAI.waitReady(listener, queue: queue)
    }

    func stop() { listener.cancel() }
}

private final class OrderBox: @unchecked Sendable {
    private let lock = NSLock()
    private var list: [Int] = []
    func add(_ v: Int) { lock.withLock { list.append(v) } }
    var values: [Int] { lock.withLock { list } }
}

private final class DispositionBox: @unchecked Sendable {
    private let lock = NSLock()
    private var got: (URLSession.AuthChallengeDisposition, URLCredential?)?
    func set(_ d: URLSession.AuthChallengeDisposition, _ c: URLCredential?) { lock.withLock { got = (d, c) } }
    var value: (URLSession.AuthChallengeDisposition, URLCredential?)? { lock.withLock { got } }
}

private final class NoSender: NSObject, URLAuthenticationChallengeSender {
    func use(_ credential: URLCredential, for challenge: URLAuthenticationChallenge) {}
    func continueWithoutCredential(for challenge: URLAuthenticationChallenge) {}
    func cancel(_ challenge: URLAuthenticationChallenge) {}
}
