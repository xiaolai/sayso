import Foundation

/// One realtime WebSocket (spec/session.md, `NET-03`): one reader feeds ``next()`` one message at a time, and no
/// client pings are sent: liveness comes from the caller's own timers.
///
/// Every message is delivered with its bytes, including events this package ignores or does not know. Only the unknown
/// ones are also counted, in ``unknownCounts``, as API drift: an ignored type is documented, not drift. A malformed
/// event fails only its own ``next()`` call as `protocol`; a message over the read limit, a close frame or a broken
/// connection ends the session, classified.
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
    /// The task's delegate: it reports when the transport has completed.
    private let events: Handshake
    private let inbox: Inbox
    private let writes = WriteGate()
    private let reader: Task<Void, Never>

    private init(client: HTTPClient, task: URLSessionWebSocketTask, events: Handshake, readLimit: Int) {
        let inbox = Inbox()
        self.client = client
        self.task = task
        self.events = events
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
        return RealtimeSession(client: client, task: task, events: handshake, readLimit: configuration.readLimit)
    }

    /// The next message. Cancelling the calling task ends the wait, not the session, and loses no message.
    public func next() async throws -> RealtimeMessage {
        try await inbox.take()
    }

    /// Writes one client event, an encoded JSON object, as a text message. Writes are serialised, as in Go, and
    /// ``close()`` releases every write still waiting with ``Closed``. A task cancelled before its write starts, while
    /// it waits behind another write included, throws `CancellationError`, sends nothing and leaves the session open.
    /// Cancelling a write in flight ends the session, as in Go: a message partly handed to the transport cannot be
    /// withdrawn, and a stalled write must not hold the caller.
    public func send(_ event: Data) async throws {
        if inbox.isClosing { throw Closed() }
        try Task.checkCancellation()
        try await writes.acquire()
        defer { writes.release() }
        if inbox.isClosing { throw Closed() }  // closed while this write waited
        let task = self.task
        do {
            try await Self.admitted { try await task.send(.string(String(decoding: event, as: UTF8.self))) } end: {
                task.cancel(with: .goingAway, reason: nil)
            }
        } catch {
            if inbox.isClosing { throw Closed() }
            if error is CancellationError || Task.isCancelled { throw CancellationError() }
            throw transportError(error, "realtime send")
        }
    }

    /// Runs `write` unless the calling task is cancelled first, and calls `end` when it is cancelled once `write` has
    /// started. Cancellation and the start are settled under one lock (``WriteAdmission``), so a cancellation that
    /// comes first, even one that lands as the handler is installed, never touches the transport.
    static func admitted(_ write: () async throws -> Void, end: @escaping @Sendable () -> Void) async throws {
        let admission = WriteAdmission()
        try await withTaskCancellationHandler {
            guard admission.start() else { throw CancellationError() }
            defer { admission.finish() }
            try await write()
        } onCancel: {
            if admission.cancel() { end() }
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

    /// How many events of each unknown type this session received, API drift: one entry per distinct type as sent,
    /// in the order first seen. Types are told apart by their UTF-8 bytes, as in Go: two canonically equivalent
    /// spellings are two types on the wire, so a `String`-keyed dictionary, which would merge them, is not used.
    public var unknownCounts: [UnknownTypeCount] { inbox.unknownCounts }

    /// Sends a normal close and drops the connection if the peer has not finished closing within 2 s, then waits for
    /// the transport to complete, as Go waits for its connection: a dropped task always completes. It waits for the
    /// reader too, so nothing outlives it. It runs to the end even when the calling task is cancelled. Safe to call
    /// more than once.
    public func close() async {
        inbox.markClosing()
        writes.close()
        task.cancel(with: .normalClosure, reason: nil)
        let reader = self.reader
        let task = self.task
        let events = self.events
        // An unstructured task does not inherit the caller's cancellation, so the grace really waits: in a cancelled
        // caller Task.sleep would throw at once and drop the connection without a close handshake.
        await Task {
            let dropped = Task {
                let clock = ContinuousClock()
                let grace = clock.now.advanced(by: Deadline.duration(Self.closeGrace) ?? .zero)
                try? await Task.sleep(until: grace, clock: clock)
                if !Task.isCancelled { task.cancel() }
            }
            // The deadline runs until URLSession reports the task complete, not until the reader ends: a buffered
            // message releases the reader at once, before the peer has answered the close.
            await events.completion()
            dropped.cancel()
            await reader.value
        }.value
    }

    /// Whether URLSession has reported the task complete; for tests.
    var transportCompleted: Bool { events.isComplete }

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

/// How many events of one unknown type a session received; see ``RealtimeSession/unknownCounts``.
public struct UnknownTypeCount: Sendable, Equatable {
    /// The type as sent.
    public var type: String
    public var count: Int

    public init(type: String, count: Int) {
        self.type = type
        self.count = count
    }

    /// Byte identity, as the counts are kept: canonically equivalent spellings are different types.
    public static func == (a: UnknownTypeCount, b: UnknownTypeCount) -> Bool {
        a.type.utf8.elementsEqual(b.type.utf8) && a.count == b.count
    }
}

/// Serialises writes. A waiter cancelled before its turn leaves the queue and never writes; the transport is not
/// touched, so a cancelled send queued behind a stalled one does not end the session. Once closed, every waiter, now
/// and later, gets ``RealtimeSession/Closed``, as Go's wait selects on the session closing.
final class WriteGate: @unchecked Sendable {
    private let lock = NSLock()
    private var busy = false
    private var closed = false
    private var waiters: [UInt64: CheckedContinuation<Void, Error>] = [:]
    /// Waiters in arrival order. A cancelled waiter leaves only `waiters`; its id is skipped when it reaches the front.
    private var order = FIFO<UInt64>()
    private var nextID: UInt64 = 0

    func acquire() async throws {
        let id: UInt64 = lock.withLock {
            defer { nextID += 1 }
            return nextID
        }
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
                lock.lock()
                if closed {
                    lock.unlock()
                    c.resume(throwing: RealtimeSession.Closed())
                } else if Task.isCancelled {
                    lock.unlock()
                    c.resume(throwing: CancellationError())
                } else if !busy {
                    busy = true
                    lock.unlock()
                    c.resume()
                } else {
                    waiters[id] = c
                    order.append(id)
                    lock.unlock()
                }
            }
        } onCancel: {
            let waiter = lock.withLock { waiters.removeValue(forKey: id) }
            waiter?.resume(throwing: CancellationError())
        }
    }

    /// How many waiters are queued; for tests.
    var waiting: Int { lock.withLock { waiters.count } }

    /// Hands the turn to the next waiter, or frees the gate.
    func release() {
        lock.lock()
        while let id = order.popFirst() {
            if let next = waiters.removeValue(forKey: id) {
                lock.unlock()
                next.resume()
                return
            }
        }
        busy = false
        lock.unlock()
    }

    /// Ends the gate for ``RealtimeSession/close()``. The write in flight, if any, ends with the transport.
    func close() {
        lock.lock()
        closed = true
        var pending: [CheckedContinuation<Void, Error>] = []
        while let id = order.popFirst() {
            if let w = waiters.removeValue(forKey: id) { pending.append(w) }
        }
        lock.unlock()
        for w in pending { w.resume(throwing: RealtimeSession.Closed()) }
    }
}

/// Settles a write's start against its cancellation: whichever comes first under the lock decides whether the
/// transport is touched.
final class WriteAdmission: @unchecked Sendable {
    private enum State { case waiting, writing, settled }
    private let lock = NSLock()
    private var state = State.waiting

    /// True when the write may start: no cancellation came first.
    func start() -> Bool {
        lock.withLock {
            guard state == .waiting else { return false }
            state = .writing
            return true
        }
    }

    /// The write has returned; a later cancellation touches nothing.
    func finish() { lock.withLock { state = .settled } }

    /// True when a write is in flight and must be ended; otherwise no write starts after this.
    func cancel() -> Bool {
        lock.withLock {
            let writing = state == .writing
            if state == .waiting { state = .settled }
            return writing
        }
    }
}

/// The hand-off between the reader and ``RealtimeSession/next()``: at most one message waits, so the reader applies
/// back-pressure as an unbuffered channel would.
final class Inbox: @unchecked Sendable {
    private let lock = NSLock()
    private var buffered: Result<RealtimeMessage, Error>?
    private var terminal: Error?
    private var closing = false
    private var waiters: [UInt64: CheckedContinuation<RealtimeMessage, Error>] = [:]
    /// Waiters in arrival order. A cancelled waiter leaves only `waiters`; its id is skipped when it reaches the front.
    private var order = FIFO<UInt64>()
    private var nextID: UInt64 = 0
    private var space: CheckedContinuation<Bool, Never>?
    private var unknown: [UnknownTypeCount] = []
    private var unknownIndex: [[UInt8]: Int] = [:]

    var isClosing: Bool {
        lock.lock()
        defer { lock.unlock() }
        return closing
    }

    var unknownCounts: [UnknownTypeCount] {
        lock.lock()
        defer { lock.unlock() }
        return unknown
    }

    func countUnknown(_ type: String) {
        let key = Array(type.utf8)
        lock.lock()
        if let at = unknownIndex[key] {
            unknown[at].count += 1
        } else {
            unknownIndex[key] = unknown.count
            unknown.append(UnknownTypeCount(type: type, count: 1))
        }
        lock.unlock()
    }

    /// The waiters in arrival order, taken from the queue; called under the lock.
    private func takeWaiters() -> [CheckedContinuation<RealtimeMessage, Error>] {
        var pending: [CheckedContinuation<RealtimeMessage, Error>] = []
        while let id = order.popFirst() {
            if let w = waiters.removeValue(forKey: id) { pending.append(w) }
        }
        return pending
    }

    /// Ends the inbox for ``RealtimeSession/close()``: a buffered message is dropped, and every waiter, now and later,
    /// gets ``RealtimeSession/Closed``.
    func markClosing() {
        lock.lock()
        closing = true
        buffered = nil
        terminal = RealtimeSession.Closed()  // after close every call gets Closed, an earlier failure included, as in Go
        let error = terminal!
        let pending = takeWaiters()
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
        while let id = order.popFirst() {
            if let waiter = waiters.removeValue(forKey: id) {
                lock.unlock()
                waiter.resume(with: result)
                return
            }
        }
        buffered = result
        lock.unlock()
    }

    /// Ends the session: every waiter, now and later, gets `error`.
    func finish(_ error: Error) {
        lock.lock()
        if terminal == nil { terminal = error }  // after close(), the reader's own error is not the session's
        let error = terminal!
        let pending = takeWaiters()
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
            lock.unlock()
            waiter?.resume(throwing: CancellationError())
        }
    }
}

/// The task's delegate. It settles the opening handshake (open, or the task's completion with an error), reports the
/// task's completion to ``RealtimeSession/close()``, and refuses redirects.
private final class Handshake: NSObject, URLSessionWebSocketDelegate, @unchecked Sendable {
    private let lock = NSLock()
    private var result: Result<Void, Error>?
    private var waiter: CheckedContinuation<Void, Error>?
    private var completed = false
    private var completionWaiters: [CheckedContinuation<Void, Never>] = []

    var isComplete: Bool { lock.withLock { completed } }

    /// Returns once URLSession has reported the task complete.
    func completion() async {
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            lock.lock()
            if completed {
                lock.unlock()
                c.resume()
            } else {
                completionWaiters.append(c)
                lock.unlock()
            }
        }
    }

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
        lock.lock()
        completed = true
        let waiters = completionWaiters
        completionWaiters = []
        lock.unlock()
        for w in waiters { w.resume() }
    }

    func urlSession(
        _ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
    ) { completionHandler(nil) }

    func urlSession(
        _ session: URLSession, task: URLSessionTask, didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) { answerChallenge(challenge, completionHandler) }
}
