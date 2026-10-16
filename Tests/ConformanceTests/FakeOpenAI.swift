import Foundation
import Network
import RealtimeConnection

/// Plain-HTTP and WebSocket stand-ins for OpenAI on 127.0.0.1, reached through an origin the client's policy admits
/// explicitly as a test origin, so the policy still checks every bearer request.
enum FakeOpenAI {
    /// A client whose policy admits only the server at `port`.
    static func client(port: UInt16) throws -> HTTPClient {
        HTTPClient(policy: try OriginPolicy(testOrigins: ["127.0.0.1:\(port)"]))
    }

    static func loopbackParameters(_ base: NWParameters) -> NWParameters {
        base.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        base.allowLocalEndpointReuse = true
        return base
    }

    static func waitReady(_ listener: NWListener, queue: DispatchQueue) throws -> UInt16 {
        let ready = DispatchSemaphore(value: 0)
        let failed = FakeOpenAI.Flag()
        listener.stateUpdateHandler = { state in
            switch state {
            case .ready: ready.signal()
            case .failed, .cancelled:
                failed.set()
                ready.signal()
            default: break
            }
        }
        listener.start(queue: queue)
        guard ready.wait(timeout: .now() + 10) == .success, !failed.value, let port = listener.port?.rawValue else {
            listener.cancel()
            throw FixtureError("the fake server did not start")
        }
        return port
    }
}

/// One scripted HTTP reply.
struct FakeHTTPReply: Sendable {
    let status: Int
    let headers: [(String, String)]
    let body: Data
}

/// One request as the fake server received it.
struct FakeHTTPRequest: Sendable {
    let method: String
    let path: String
    let headers: [String: String]  // lower-cased names
    let body: Data
}

/// An HTTP/1.1 server that answers the n-th request with the n-th reply (418 when the script has none) and closes
/// each connection after its reply.
final class FakeHTTPServer: @unchecked Sendable {
    private let listener: NWListener
    private let queue = DispatchQueue(label: "fake-openai-http")
    private let lock = NSLock()
    private var seen: [FakeHTTPRequest] = []
    private let replies: [FakeHTTPReply]

    init(replies: [FakeHTTPReply]) throws {
        self.replies = replies
        listener = try NWListener(using: FakeOpenAI.loopbackParameters(.tcp))
    }

    /// Starts listening and returns the port.
    func start() throws -> UInt16 {
        listener.newConnectionHandler = { [self] connection in serve(connection) }
        return try FakeOpenAI.waitReady(listener, queue: queue)
    }

    var requests: [FakeHTTPRequest] {
        lock.lock()
        defer { lock.unlock() }
        return seen
    }

    func stop() { listener.cancel() }

    private func serve(_ connection: NWConnection) {
        connection.start(queue: queue)
        receive(connection, Data())
    }

    private func receive(_ connection: NWConnection, _ buffer: Data) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { data, _, complete, error in
            var buffer = buffer
            if let data { buffer.append(data) }
            if let request = Self.parse(buffer) {
                self.answer(connection, request)
            } else if complete || error != nil {
                connection.cancel()
            } else {
                self.receive(connection, buffer)
            }
        }
    }

    /// The request in `buffer` once its headers and its Content-Length body have arrived.
    private static func parse(_ buffer: Data) -> FakeHTTPRequest? {
        guard let end = buffer.range(of: Data("\r\n\r\n".utf8)) else { return nil }
        let head = String(decoding: buffer[buffer.startIndex..<end.lowerBound], as: UTF8.self)
        var lines = head.components(separatedBy: "\r\n")
        let requestLine = lines.removeFirst().split(separator: " ")
        guard requestLine.count >= 2 else { return nil }
        var headers: [String: String] = [:]
        for line in lines {
            guard let colon = line.firstIndex(of: ":") else { continue }
            headers[line[..<colon].lowercased()] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        let length = Int(headers["content-length"] ?? "0") ?? 0
        let body = buffer[end.upperBound...]
        guard body.count >= length else { return nil }
        let target = String(requestLine[1])
        let path = target.split(separator: "?", maxSplits: 1).first.map(String.init) ?? target
        return FakeHTTPRequest(
            method: String(requestLine[0]), path: path, headers: headers, body: Data(body.prefix(length)))
    }

    private func answer(_ connection: NWConnection, _ request: FakeHTTPRequest) {
        lock.lock()
        let n = seen.count
        seen.append(request)
        lock.unlock()
        let reply =
            n < replies.count
            ? replies[n] : FakeHTTPReply(status: 418, headers: [], body: Data("fixture has no reply for this request".utf8))
        var head = "HTTP/1.1 \(reply.status) Fixture\r\n"
        for (k, v) in reply.headers { head += "\(k): \(v)\r\n" }
        head += "Content-Length: \(reply.body.count)\r\nConnection: close\r\n\r\n"
        var out = Data(head.utf8)
        out.append(reply.body)
        connection.send(content: out, completion: .contentProcessed { _ in connection.cancel() })
    }
}

/// A TCP server that reads one HTTP request head, records it, and answers it with `reply` as raw bytes, closing the
/// connection unless `hold`; with no reply it never answers. Every connection it holds ends with ``stop()``.
final class FakeHandshakeServer: @unchecked Sendable {
    /// What the connection handler records, made before the listener so its handler can hold it.
    private final class Record: @unchecked Sendable {
        let lock = NSLock()
        var head: String?
        var held: [NWConnection] = []
    }

    private let listener: NWListener
    private let queue = DispatchQueue(label: "fake-openai-handshake")
    private let record = Record()
    let port: UInt16

    init(reply: Data?, hold: Bool) throws {
        listener = try NWListener(using: FakeOpenAI.loopbackParameters(.tcp))
        let (queue, record) = (self.queue, self.record)
        listener.newConnectionHandler = { connection in
            connection.start(queue: queue)
            Self.receive(connection, Data(), record, reply: reply, hold: hold)
        }
        port = try FakeOpenAI.waitReady(listener, queue: queue)
    }

    private static func receive(_ connection: NWConnection, _ buffer: Data, _ record: Record, reply: Data?, hold: Bool) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 1 << 16) { data, _, complete, error in
            var buffer = buffer
            if let data { buffer.append(data) }
            guard let end = buffer.range(of: Data("\r\n\r\n".utf8)) else {
                if complete || error != nil {
                    connection.cancel()
                } else {
                    receive(connection, buffer, record, reply: reply, hold: hold)
                }
                return
            }
            record.lock.withLock {
                record.head = String(decoding: buffer[buffer.startIndex..<end.lowerBound], as: UTF8.self)
                record.held.append(connection)
            }
            guard let reply else { return }  // silent: held until stop
            connection.send(content: reply, completion: .contentProcessed { _ in if !hold { connection.cancel() } })
        }
    }

    private var lines: [String] { record.lock.withLock { record.head }?.components(separatedBy: "\r\n") ?? [] }

    /// The request line's target, once a request has arrived.
    var target: String? {
        let parts = lines.first?.split(separator: " ") ?? []
        return parts.count >= 2 ? String(parts[1]) : nil
    }

    /// The request's Authorization header.
    var authorization: String? {
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":"), line[..<colon].lowercased() == "authorization" else { continue }
            return line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        return nil
    }

    func stop() {
        listener.cancel()
        for connection in record.lock.withLock({ record.held }) { connection.cancel() }
    }
}

/// A WebSocket server that, on each connection, sends `message` as one text frame and then waits for the client.
final class FakeWebSocketServer: @unchecked Sendable {
    private let listener: NWListener
    private let queue = DispatchQueue(label: "fake-openai-ws")
    let port: UInt16

    convenience init(message: Data) throws { try self.init(messages: [message]) }

    /// Sends `messages` in order to each client, `after` seconds once it is connected, then waits for the client's
    /// first message or close.
    init(messages: [Data], after delay: TimeInterval = 0) throws {
        let parameters = FakeOpenAI.loopbackParameters(NWParameters(tls: nil, tcp: .init()))
        let options = NWProtocolWebSocket.Options()
        options.autoReplyPing = true
        options.maximumMessageSize = 64 << 20
        parameters.defaultProtocolStack.applicationProtocols.insert(options, at: 0)
        listener = try NWListener(using: parameters)
        let queue = self.queue
        listener.newConnectionHandler = { connection in
            connection.stateUpdateHandler = { state in
                guard case .ready = state else { return }
                let metadata = NWProtocolWebSocket.Metadata(opcode: .text)
                let context = NWConnection.ContentContext(identifier: "message", metadata: [metadata])
                queue.asyncAfter(deadline: .now() + delay) {
                    for message in messages {
                        connection.send(
                            content: message, contentContext: context, isComplete: true, completion: .idempotent)
                    }
                }
                connection.receiveMessage { _, _, _, _ in connection.cancel() }
            }
            connection.start(queue: queue)
        }
        port = try FakeOpenAI.waitReady(listener, queue: queue)
    }

    func stop() { listener.cancel() }
}

extension FakeOpenAI {
    final class ResultBox<T>: @unchecked Sendable {
        var result: Result<T, Error>?
    }

    final class Flag: @unchecked Sendable {
        private let lock = NSLock()
        private var on = false
        func set() { lock.withLock { on = true } }
        var value: Bool { lock.withLock { on } }
    }

    /// Runs async work from a synchronous fixture check, on the cooperative pool, and waits for it; work still running
    /// after ``caseWait`` is cancelled and fails the case.
    static func blocking<T: Sendable>(_ operation: @escaping @Sendable () async throws -> T) -> Result<T, Error> {
        let box = ResultBox<T>()
        let done = DispatchSemaphore(value: 0)
        let task = Task.detached {
            do { box.result = .success(try await operation()) } catch { box.result = .failure(error) }
            done.signal()
        }
        guard done.waited(caseWait) else {
            task.cancel()  // the box is left to the task: it is read only after the signal
            return .failure(FixtureError("the case's async work did not finish within \(Int(caseWait)) s"))
        }
        return box.result ?? .failure(FixtureError("no result"))
    }

    /// Comparison of a Swift value and a decoded fixture value by their JSON meaning.
    static func normal(_ value: Any?) -> Any {
        let v: Any = value ?? NSNull()
        guard JSONSerialization.isValidJSONObject([v]), let data = try? JSONSerialization.data(withJSONObject: [v]),
            let back = try? JSONSerialization.jsonObject(with: data) as? [Any], let first = back.first
        else { return "unencodable \(type(of: v))" }
        return first
    }

    static func same(_ got: Any?, _ want: Any?) -> Bool {
        (normal(got) as AnyObject).isEqual(normal(want))
    }

    static func strings(_ value: Any?) -> [String] {
        (value as? [Any])?.map { "\($0)" } ?? []
    }

    /// The strings of `notIn` that appear in `text`.
    static func leaks(_ notIn: [String], _ text: String) -> [String] {
        notIn.filter { text.contains($0) }.map { "\"\($0)\" leaked" }
    }

    /// Compares a classified error with class, fatal, code, type, param and notInDescription.
    static func checkError(_ error: Error?, _ want: [String: Any]) -> [String] {
        guard let e = error as? RealtimeError else { return ["not a classified error: \(String(describing: error))"] }
        var p: [String] = []
        if let w = want["class"], !same(e.errorClass.rawValue, w) { p.append("class \(e.errorClass.rawValue), want \(w)") }
        if let w = want["fatal"], !same(e.fatal, w) { p.append("fatal \(e.fatal), want \(w)") }
        for (key, got) in [("code", e.code), ("type", e.type), ("param", e.param)] {
            if let w = want[key], !same(got, w) { p.append("\(key) \"\(got)\", want \(w)") }
        }
        return p + leaks(strings(want["notInDescription"]), String(describing: e))
    }

    /// The members every case may carry besides its component's own (spec/fixtures.md).
    static let caseHead: Set<String> = ["id", "component", "note", "requires"]

    /// Each key of `m` outside `known`, sorted, as a problem in `place`: a member no harness reads would let a case
    /// pass whatever it says.
    static func unknownKeys(_ place: String, _ m: [String: Any]?, _ known: Set<String>) -> [String] {
        (m ?? [:]).keys.filter { !known.contains($0) }.sorted().map { "unknown \(place) key \"\($0)\"" }
    }

    /// The expectation keys ``checkError(_:_:)`` reads.
    static let errorKeys: Set<String> = ["class", "fatal", "code", "type", "param", "notInDescription"]

    static func outcome(_ problems: [String]) -> String? {
        problems.isEmpty ? nil : problems.joined(separator: "; ")
    }
}
