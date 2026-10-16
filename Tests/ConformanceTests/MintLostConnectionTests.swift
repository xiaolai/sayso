import Foundation
import Network
import XCTest

import RealtimeConnection

/// A mint over a kept-alive connection the server has closed (spec/http-and-mint.md, lost connection). The shared
/// fixtures hang up on fresh connections; these keep each connection open between mints, as the real origin does.
final class MintLostConnectionTests: XCTestCase {
    /// The server closes the idle connection after the first reply: the second mint goes over a new one.
    func testAMintAfterTheServerClosedTheIdleConnectionSucceeds() async throws {
        let server = try KeepAliveServer(idleClose: true) { _ in false }
        defer { server.stop() }
        let (first, second) = try await twoMints(server)
        XCTAssertNil(first)
        XCTAssertNil(second)
        XCTAssertEqual(server.requests, 2)
    }

    /// The server reads the second mint on the kept connection and closes it without a reply: the mint is sent once
    /// more and succeeds.
    func testAMintLostOnAKeptConnectionIsSentOnceMore() async throws {
        let server = try KeepAliveServer(idleClose: false) { n in n == 2 }
        defer { server.stop() }
        let (first, second) = try await twoMints(server)
        XCTAssertNil(first)
        XCTAssertNil(second, "the lost mint was not sent again")
        XCTAssertEqual(server.requests, 3)
    }

    /// A second loss in a row is returned as `network`; the mint is not sent a third time.
    func testASecondLossIsNotSentAgain() async throws {
        let server = try KeepAliveServer(idleClose: false) { n in n == 2 || n == 3 }
        defer { server.stop() }
        let (first, second) = try await twoMints(server)
        XCTAssertNil(first)
        XCTAssertEqual((second as? RealtimeError)?.errorClass, .network, "\(String(describing: second))")
        XCTAssertEqual((second as? RealtimeError)?.cause, "URLError \(URLError.Code.networkConnectionLost.rawValue)")
        XCTAssertEqual(server.requests, 3)
    }

    /// Mints twice on one client, 200 ms apart; each result is nil on success or the error.
    private func twoMints(_ server: KeepAliveServer) async throws -> (Error?, Error?) {
        let port = try server.start()
        let minter = Minter(
            client: try FakeOpenAI.client(port: port), credentials: FixedBearer(), timeout: 5, lifetimeSeconds: 600,
            baseURL: URL(string: "http://127.0.0.1:\(port)")!)
        func mint() async -> Error? {
            do {
                _ = try await minter.secret(session: Data(#"{"type":"realtime"}"#.utf8))
                return nil
            } catch { return error }
        }
        let first = await mint()
        try await Task.sleep(nanoseconds: 200_000_000)
        return (first, await mint())
    }
}

private struct FixedBearer: CredentialSource {
    func token() async throws -> Token { Token(bearer: "bearer-lost", expires: nil, source: "test") }
    func refresh(after stale: Token) async throws -> Token { try await token() }
}

/// An HTTP/1.1 server that keeps every connection open between requests. `drop(n)` says whether the n-th request
/// (from 1, across every connection) is read and met with a closed connection and no reply; with `idleClose`, each
/// connection is closed 50 ms after a reply, while it is idle.
private final class KeepAliveServer: @unchecked Sendable {
    private let listener: NWListener
    private let queue = DispatchQueue(label: "keep-alive-server")
    private let lock = NSLock()
    private var count = 0
    private let drop: @Sendable (Int) -> Bool
    private let idleClose: Bool

    init(idleClose: Bool, drop: @escaping @Sendable (Int) -> Bool) throws {
        self.idleClose = idleClose
        self.drop = drop
        listener = try NWListener(using: FakeOpenAI.loopbackParameters(.tcp))
    }

    func start() throws -> UInt16 {
        listener.newConnectionHandler = { [self] connection in
            connection.start(queue: queue)
            receive(connection, Data())
        }
        return try FakeOpenAI.waitReady(listener, queue: queue)
    }

    func stop() { listener.cancel() }

    var requests: Int { lock.withLock { count } }

    private func receive(_ connection: NWConnection, _ buffer: Data) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { [self] data, _, complete, error in
            var buffer = buffer
            if let data { buffer.append(data) }
            if let end = Self.requestEnd(buffer) {
                buffer.removeSubrange(buffer.startIndex..<end)
                answer(connection, rest: buffer)
            } else if complete || error != nil {
                connection.cancel()
            } else {
                receive(connection, buffer)
            }
        }
    }

    /// The index just past the first complete request in `buffer`, once its headers and Content-Length body arrived.
    private static func requestEnd(_ buffer: Data) -> Data.Index? {
        guard let head = buffer.range(of: Data("\r\n\r\n".utf8)) else { return nil }
        var length = 0
        for line in String(decoding: buffer[buffer.startIndex..<head.lowerBound], as: UTF8.self)
            .components(separatedBy: "\r\n")
        where line.lowercased().hasPrefix("content-length:") {
            length = Int(line.dropFirst("content-length:".count).trimmingCharacters(in: .whitespaces)) ?? 0
        }
        let end = head.upperBound + length
        return buffer.endIndex >= end ? end : nil
    }

    private func answer(_ connection: NWConnection, rest: Data) {
        let n = lock.withLock {
            count += 1
            return count
        }
        if drop(n) {
            connection.cancel()
            return
        }
        let body = #"{"value":"ek_kept","expires_at":1900000000}"#
        let reply = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: \(body.utf8.count)\r\n\r\n" + body
        connection.send(
            content: Data(reply.utf8),
            completion: .contentProcessed { [self] _ in
                if idleClose {
                    queue.asyncAfter(deadline: .now() + 0.05) { connection.cancel() }
                } else {
                    receive(connection, rest)
                }
            })
    }
}
