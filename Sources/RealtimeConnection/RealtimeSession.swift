import Foundation

/// One realtime WebSocket (spec/session.md, `NET-03`): one reader feeds ``next()`` one message at a time, and no
/// client pings are sent: liveness comes from the caller's own timers.
///
/// Every message is delivered with its bytes, including events this package ignores or does not know, which are also
/// counted in ``unknownCounts`` as API drift. A malformed event fails only its own ``next()`` call as `protocol`; a
/// message over the read limit, a close frame or a broken connection ends the session, classified.
public final class RealtimeSession: Sendable {
    /// The realtime endpoint for one session.
    public struct Configuration: Sendable {
        public var url: URL
        /// Bounds the opening handshake, in seconds; required.
        public var handshakeTimeout: TimeInterval
        /// The largest message accepted, in bytes; required. A longer message is `protocol` and is never decoded.
        public var readLimit: Int
        /// Named on a route-denied handshake refusal; optional.
        public var route: String

        public init(url: URL, handshakeTimeout: TimeInterval, readLimit: Int, route: String = "") {
            self.url = url
            self.handshakeTimeout = handshakeTimeout
            self.readLimit = readLimit
            self.route = route
        }
    }

    /// Returned by ``next()`` and ``send(_:)`` after ``close()``.
    public struct Closed: Error, Sendable, Equatable {
        public init() {}
    }

    /// The realtime WebSocket origin.
    public static let apiBase = URL(string: "wss://api.openai.com")!

    /// The realtime endpoint for `model` under `base` (``apiBase`` in production).
    public static func url(model: String, base: URL = apiBase) -> URL {
        var s = base.absoluteString
        while s.hasSuffix("/") { s.removeLast() }
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        let escaped = model.addingPercentEncoding(withAllowedCharacters: allowed) ?? model
        return URL(string: s + "/v1/realtime?model=" + escaped) ?? base
    }

    /// How long ``close()`` waits for the close handshake before dropping the connection.
    static let closeGrace: TimeInterval = 2

    /// Kept so its URLSession, which owns the connection, lives as long as the session.
    private let client: HTTPClient
    private let task: URLSessionWebSocketTask
    private let inbox: Inbox
    private let reader: Task<Void, Never>

    private init(client: HTTPClient, task: URLSessionWebSocketTask, readLimit: Int) {
        let inbox = Inbox()
        self.client = client
        self.task = task
        self.inbox = inbox
        self.reader = Task.detached { await Self.read(task, inbox, readLimit) }
    }

    deinit {
        inbox.markClosing()
        task.cancel(with: .goingAway, reason: nil)
    }

    /// Obtains a secret for `session` (an encoded JSON object) from `source` and opens the session with it.
    public static func open(
        client: HTTPClient, source: any SecretSource, session: Data, configuration: Configuration
    ) async throws -> RealtimeSession {
        let secret = try await source.secret(session: session)
        return try await open(client: client, secret: secret, configuration: configuration)
    }

    /// Opens the session with `Authorization: Bearer ek_…` within the handshake timeout. A refused handshake is
    /// classified like an HTTP reply.
    public static func open(client: HTTPClient, secret: ClientSecret, configuration: Configuration) async throws
        -> RealtimeSession
    {
        guard Deadline.nanoseconds(configuration.handshakeTimeout) != nil, configuration.readLimit > 0,
            configuration.readLimit < Int.max
        else {
            throw RealtimeError(
                .internal, message: "a realtime session needs a positive, finite handshake timeout and a read limit")
        }
        var request = URLRequest(url: configuration.url)
        request.setValue("Bearer " + secret.bearer, forHTTPHeaderField: "Authorization")
        request.timeoutInterval = configuration.handshakeTimeout
        try client.policy.check(request)
        let task = client.session.webSocketTask(with: request)
        // One byte over the limit still arrives, so the session tells an oversize message from a broken connection.
        task.maximumMessageSize = configuration.readLimit + 1
        let handshake = Handshake()
        task.delegate = handshake
        task.resume()
        do {
            try await withDeadline(seconds: configuration.handshakeTimeout, what: "realtime handshake") {
                try await handshake.wait()
            }
        } catch {
            task.cancel()
            // A reply without a valid upgrade is classified from its status and request id (spec/session.md):
            // URLSession never exposes a refused handshake's body. A 2xx, or a 101 it refused, is protocol.
            if !Task.isCancelled, let http = task.response as? HTTPURLResponse {
                let requestID = http.value(forHTTPHeaderField: "x-request-id") ?? ""
                if http.statusCode == 101 {
                    throw RealtimeError(
                        .protocol, status: 101, message: "realtime handshake: not a WebSocket upgrade",
                        requestID: requestID)
                }
                let refused = HTTPClient.errorReply(
                    status: http.statusCode, requestID: requestID, body: Data(), route: configuration.route)
                if refused.errorClass == .protocol {
                    throw RealtimeError(
                        .protocol, status: http.statusCode, message: "realtime handshake: not a WebSocket upgrade",
                        requestID: requestID)
                }
                throw refused
            }
            throw error
        }
        return RealtimeSession(client: client, task: task, readLimit: configuration.readLimit)
    }

    /// The next message. Cancelling the calling task ends the wait, not the session, and loses no message.
    public func next() async throws -> RealtimeMessage {
        try await inbox.take()
    }

    /// Writes one client event, an encoded JSON object, as a text message.
    public func send(_ event: Data) async throws {
        if inbox.isClosing { throw Closed() }
        do {
            try await task.send(.string(String(decoding: event, as: UTF8.self)))
        } catch {
            if inbox.isClosing { throw Closed() }
            throw transportError(error, "realtime send")
        }
    }

    /// Writes one `Encodable` client event, encoded without escaping slashes.
    public func send<Event: Encodable>(encoding event: Event) async throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        let data: Data
        do { data = try encoder.encode(event) } catch {
            throw RealtimeError(.internal, message: "the event does not encode")
        }
        try await send(data)
    }

    /// How many events of each unknown type this session received: API drift.
    public var unknownCounts: [String: Int] { inbox.unknownCounts }

    /// Sends a normal close and drops the connection if the peer has not finished closing within 2 s. It waits for
    /// the reader, so nothing outlives it. Safe to call more than once.
    public func close() async {
        inbox.markClosing()
        task.cancel(with: .normalClosure, reason: nil)
        let reader = self.reader
        let task = self.task
        let dropped = Task {
            try? await Task.sleep(nanoseconds: Deadline.nanoseconds(Self.closeGrace) ?? 0)
            if !Task.isCancelled { task.cancel() }
        }
        await reader.value
        dropped.cancel()
    }

    /// The error for a close frame received while more events were expected; the reason is scrubbed.
    public static func closeError(code: Int, reason: String) -> RealtimeError {
        RealtimeError.connectionClosed(code: code, reason: reason)
    }

    /// The only reader. It ends when the connection fails or ``close()`` runs.
    private static func read(_ task: URLSessionWebSocketTask, _ inbox: Inbox, _ readLimit: Int) async {
        while await inbox.waitForSpace() {
            let message: URLSessionWebSocketTask.Message
            do {
                message = try await task.receive()
            } catch {
                inbox.finish(readError(task, inbox, error))
                return
            }
            let bytes: Data
            switch message {
            case .string(let text): bytes = Data(text.utf8)
            case .data(let data): bytes = data
            @unknown default: bytes = Data()
            }
            if bytes.count > readLimit {
                inbox.finish(
                    RealtimeError(.protocol, code: "1009", message: "realtime message exceeds the read limit"))
                task.cancel(with: .messageTooBig, reason: nil)
                return
            }
            do {
                let decoded = try RealtimeMessage(raw: bytes)
                if case .unknown(let type) = decoded.event { inbox.countUnknown(type) }
                inbox.push(.success(decoded))
            } catch {
                inbox.push(.failure(error))
            }
        }
    }

    /// A failed read: after ``close()`` it is ``Closed``; a close frame is classified by its code; anything else is a
    /// broken connection.
    private static func readError(_ task: URLSessionWebSocketTask, _ inbox: Inbox, _ error: Error) -> Error {
        if inbox.isClosing { return Closed() }
        let code = task.closeCode.rawValue
        if task.closeCode != .invalid {
            let reason = task.closeReason.map { String(decoding: $0, as: UTF8.self) } ?? ""
            return closeError(code: code, reason: reason)
        }
        let ns = error as NSError
        if ns.domain == NSPOSIXErrorDomain && ns.code == Int(EMSGSIZE) {
            return RealtimeError(.protocol, code: "1009", message: "realtime message exceeds the read limit")
        }
        if error is CancellationError || (error as? URLError)?.code == .cancelled {
            // Cancelled underneath, not by the caller: a dropped connection.
            return RealtimeError(.network, message: "realtime connection dropped", cause: "URLError cancelled")
        }
        return transportError(error, "realtime read")
    }
}

/// The hand-off between the reader and ``RealtimeSession/next()``: at most one message waits, so the reader applies
/// back-pressure as an unbuffered channel would.
private final class Inbox: @unchecked Sendable {
    private let lock = NSLock()
    private var buffered: Result<RealtimeMessage, Error>?
    private var terminal: Error?
    private var closing = false
    private var waiters: [UInt64: CheckedContinuation<RealtimeMessage, Error>] = [:]
    private var order: [UInt64] = []
    private var nextID: UInt64 = 0
    private var space: CheckedContinuation<Bool, Never>?
    private var unknown: [String: Int] = [:]

    var isClosing: Bool {
        lock.lock()
        defer { lock.unlock() }
        return closing
    }

    var unknownCounts: [String: Int] {
        lock.lock()
        defer { lock.unlock() }
        return unknown
    }

    func countUnknown(_ type: String) {
        lock.lock()
        unknown[type, default: 0] += 1
        lock.unlock()
    }

    /// Ends the inbox for ``RealtimeSession/close()``: a buffered message is dropped, and every waiter, now and later,
    /// gets ``RealtimeSession/Closed``.
    func markClosing() {
        lock.lock()
        closing = true
        buffered = nil
        if terminal == nil { terminal = RealtimeSession.Closed() }
        let error = terminal!
        let pending = order.compactMap { waiters.removeValue(forKey: $0) }
        order = []
        let s = space
        space = nil
        lock.unlock()
        s?.resume(returning: false)
        for w in pending { w.resume(throwing: error) }
    }

    /// Waits until the last message was taken; false once the session is closing or has ended.
    func waitForSpace() async -> Bool {
        await withCheckedContinuation { (c: CheckedContinuation<Bool, Never>) in
            lock.lock()
            if closing || terminal != nil {
                lock.unlock()
                c.resume(returning: false)
            } else if buffered == nil {
                lock.unlock()
                c.resume(returning: true)
            } else {
                space = c
                lock.unlock()
            }
        }
    }

    func push(_ result: Result<RealtimeMessage, Error>) {
        lock.lock()
        if closing || terminal != nil {  // the session ended while the reader was decoding
            lock.unlock()
            return
        }
        if let id = order.first, let waiter = waiters.removeValue(forKey: id) {
            order.removeFirst()
            lock.unlock()
            waiter.resume(with: result)
            return
        }
        buffered = result
        lock.unlock()
    }

    /// Ends the session: every waiter, now and later, gets `error`.
    func finish(_ error: Error) {
        lock.lock()
        if terminal == nil { terminal = error }  // after close(), the reader's own error is not the session's
        let error = terminal!
        let pending = order.compactMap { waiters.removeValue(forKey: $0) }
        order = []
        let s = space
        space = nil
        lock.unlock()
        s?.resume(returning: false)
        for w in pending { w.resume(throwing: error) }
    }

    func take() async throws -> RealtimeMessage {
        let id: UInt64 = lock.withLock {
            defer { nextID += 1 }
            return nextID
        }
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<RealtimeMessage, Error>) in
                lock.lock()
                if Task.isCancelled {  // before taking a message, so a cancelled call never steals one
                    lock.unlock()
                    c.resume(throwing: CancellationError())
                } else if let result = buffered {
                    buffered = nil
                    let s = space
                    space = nil
                    lock.unlock()
                    s?.resume(returning: true)
                    c.resume(with: result)
                } else if let terminal {
                    lock.unlock()
                    c.resume(throwing: terminal)
                } else {
                    waiters[id] = c
                    order.append(id)
                    lock.unlock()
                }
            }
        } onCancel: {
            lock.lock()
            let waiter = waiters.removeValue(forKey: id)
            order.removeAll { $0 == id }
            lock.unlock()
            waiter?.resume(throwing: CancellationError())
        }
    }
}

/// Settles the opening handshake: open, or the task's completion with an error. Redirects are refused.
private final class Handshake: NSObject, URLSessionWebSocketDelegate, @unchecked Sendable {
    private let lock = NSLock()
    private var result: Result<Void, Error>?
    private var waiter: CheckedContinuation<Void, Error>?

    private func settle(_ r: Result<Void, Error>) {
        lock.lock()
        guard result == nil else {
            lock.unlock()
            return
        }
        result = r
        let w = waiter
        waiter = nil
        lock.unlock()
        w?.resume(with: r)
    }

    func wait() async throws {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
                lock.lock()
                if let result {
                    lock.unlock()
                    c.resume(with: result)
                } else {
                    waiter = c
                    lock.unlock()
                }
            }
        } onCancel: {
            self.settle(.failure(CancellationError()))
        }
    }

    func urlSession(
        _ session: URLSession, webSocketTask: URLSessionWebSocketTask, didOpenWithProtocol protocol: String?
    ) { settle(.success(())) }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        settle(.failure(error ?? RealtimeError(.protocol, message: "realtime handshake ended without opening")))
    }

    func urlSession(
        _ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
    ) { completionHandler(nil) }
}
