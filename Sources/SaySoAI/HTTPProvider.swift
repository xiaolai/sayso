import Foundation

/// **An OpenAI-compatible endpoint — a cloud one or a server the reader runs — as transport only.**
///
/// No locality tier and no locality claim: the endpoint is the reader's address and this library promises nothing about
/// what serves it. The key lives in this value, in memory, and never in an error, a log line or a description.
/// Requests run on one kept `URLSession`, at temperature 0, non-streaming unless the configuration asks for a stream.
public struct HTTPProvider: AIProvider {
    /// What the reader configured: an address, a model name and, for most cloud services, a key — plus how the key
    /// travels and which token-limit field the service prefers first. **The key is never printed**: description,
    /// debug description and reflection show the address and the model only.
    public struct Configuration: Sendable, Equatable {
        public let endpoint: URL
        public let model: String
        /// In memory only; the app's Keychain storage is a later item. Absent is legal: a local server often takes none.
        public let apiKey: String?
        public let auth: AIProviderAuth
        public let tokenField: AIProviderTokenField
        public let temperature: AIProviderTemperature
        /// The most tokens one answer may take, or none named.
        public let answerBudget: AnswerBudget
        public let timeouts: Timeouts
        /// The largest answer body accepted, in bytes; at least one, or construction traps. A refusal's body keeps the
        /// fixed `HTTPProvider.errorBodyCap`.
        public let responseCap: Int
        public let delivery: Delivery

        public init(endpoint: URL, model: String, apiKey: String? = nil,
                    auth: AIProviderAuth = .bearer, tokenField: AIProviderTokenField = .completionFirst,
                    temperature: AIProviderTemperature = .zero, answerBudget: AnswerBudget = .standard,
                    timeouts: Timeouts = .standard, responseCap: Int = HTTPProvider.responseCap,
                    delivery: Delivery = .whole) {
            precondition(responseCap > 0, "a response cap must be at least one byte")
            self.endpoint = endpoint
            self.model = model
            self.apiKey = apiKey
            self.auth = auth
            self.tokenField = tokenField
            self.temperature = temperature
            self.answerBudget = answerBudget
            self.timeouts = timeouts
            self.responseCap = responseCap
            self.delivery = delivery
        }
    }

    /// **How the answer travels.** Neither choice ever returns a partial answer.
    public enum Delivery: Sendable, Equatable, Hashable {
        /// `stream: false`: the reply is one JSON body, under `responseCap`.
        case whole
        /// `stream: true`: server-sent events, put together into one answer before it is returned. Bytes keep arriving
        /// while a reasoning model works, so `idle` measures real silence. A line, one event and the answer are each
        /// capped at `responseCap`; the stream as a whole is bounded by the timeouts. A server that answers with JSON
        /// instead is read whole.
        case streamed
    }

    /// **Per-request limits.** Every request enforces them, whichever session it runs on.
    public struct Timeouts: Sendable, Equatable, Hashable {
        /// The longest wait for the next byte from the server: connecting, waiting for the headers, and between any two
        /// bytes of the body. A non-streamed answer sends nothing until it is done, so this is about the longest an
        /// answer may take.
        public let idle: Duration
        /// The most one HTTP request may take from start to finish; `nil` means no ceiling, so `idle` and the caller's
        /// cancellation are the only bounds, and a server that keeps sending bytes (keep-alive comments, say) holds the
        /// call until the caller cancels. The token-field retry is a request of its own and starts a fresh one.
        public let ceiling: Duration?

        /// `idle` of `HTTPProvider.requestTimeout`, `ceiling` of `HTTPProvider.resourceTimeout`.
        public static let standard = Timeouts(idle: .seconds(HTTPProvider.requestTimeout),
                                              ceiling: .seconds(HTTPProvider.resourceTimeout))

        /// Traps when `idle` is not positive, or when `ceiling` is present and not positive. Each is at most 100 years
        /// of 365 days, or construction traps too: the watchdog sleeps until a deadline counted in 64-bit nanoseconds.
        public init(idle: Duration, ceiling: Duration?) {
            let longest = Duration.seconds(100 * 365 * 86_400)
            precondition(idle > .zero, "an idle timeout must be positive")
            precondition(ceiling.map { $0 > .zero } ?? true, "a timeout ceiling must be positive")
            precondition(idle <= longest && ceiling.map { $0 <= longest } ?? true, "a timeout must be at most 100 years")
            self.idle = idle
            self.ceiling = ceiling
        }
    }

    /// **One answer's token budget**: a positive count, or none at all. A value at or below zero cannot be built.
    public struct AnswerBudget: Sendable, Equatable, Hashable {
        private let limit: Int?

        private init(limit: Int?) {
            self.limit = limit
        }

        /// **Sends no token field, so the server's own limit applies**, whatever that is: some servers fill in the
        /// model's maximum, some generate until the context is full, some may refuse the request. It is meant for a
        /// reader who knows their server.
        public static let endpointDefault = AnswerBudget(limit: nil)
        /// `.tokens(HTTPProvider.answerTokens)`.
        public static let standard = AnswerBudget.tokens(HTTPProvider.answerTokens)

        /// For a count written in code: traps when `count < 1`. A literal zero is the caller's bug.
        public static func tokens(_ count: Int) -> AnswerBudget {
            precondition(count > 0, "an answer budget must be at least one token")
            return AnswerBudget(limit: count)
        }

        /// For a count read from settings or user input: `nil` when `count < 1`, never a trap.
        public init?(tokens count: Int) {
            guard count > 0 else { return nil }
            self.limit = count
        }

        /// The count sent, or `nil` for `.endpointDefault`.
        public var tokenLimit: Int? { limit }
    }

    /// The default answer budget. A budget bounds one answer's cost; a reasoning model spends part of it on reasoning
    /// before it writes anything, so a small budget can truncate a good answer.
    public static let answerTokens = 1_024
    /// The probe's budget: one token is enough to prove the endpoint, the model and the key.
    public static let probeTokens = 1
    /// The default idle limit, in seconds: `Timeouts.standard`'s `idle`. A hung endpoint fails as `.timeout`, never as
    /// a hang.
    public static let requestTimeout: TimeInterval = 60
    /// The default largest answer body accepted; an answer is far smaller, and anything past the configured cap is
    /// refused as a bad shape.
    public static let responseCap = 1 << 20
    /// How much of a **refusal's** body is read for its sentence. A hostile server cannot turn a 401 into a bad shape by
    /// sending megabytes with it.
    public static let errorBodyCap = 64 << 10
    /// The default ceiling on one request, in seconds: `Timeouts.standard`'s `ceiling`.
    public static let resourceTimeout: TimeInterval = 90

    /// What `probe()` reports: the endpoint answered, and how long it took.
    public enum Probe: Sendable, Equatable {
        case ready(latency: Duration)
    }

    public let id: AIProviderID
    private let configuration: Configuration
    private let session: URLSession
    /// The endpoint's origin, recorded at construction and re-checked against every redirect.
    private let origin: Origin

    /// Fails with `.insecureEndpoint` for an `http` address that is not loopback, before anything is sent.
    ///
    /// **The configuration's timeouts hold on an injected `session` too**: every request enforces them itself. A caller
    /// that passes one (a test stand-in, a shared session) keeps that session's own limits as well, and the shorter
    /// one wins. The session this initializer creates mirrors the configured timeouts as a backstop.
    public init(configuration: Configuration, session: URLSession? = nil) throws(AIProviderError) {
        guard let origin = Origin(configuration.endpoint) else { throw .insecureEndpoint }
        guard origin.isAcceptable else { throw .insecureEndpoint }
        self.origin = origin
        self.configuration = configuration
        self.id = AIProviderID(kind: .http, endpoint: configuration.endpoint)
        if let session {
            self.session = session
        } else {
            let config = URLSessionConfiguration.ephemeral
            config.timeoutIntervalForRequest = configuration.timeouts.idle / .seconds(1)
            // URLSession's own default resource timeout, seven days, stands for "no ceiling".
            config.timeoutIntervalForResource = configuration.timeouts.ceiling.map { $0 / .seconds(1) } ?? 7 * 24 * 3_600
            self.session = URLSession(configuration: config)
        }
    }

    public func answer(to question: String) async throws(AIProviderError) -> String {
        let body = Body(model: configuration.model, question: question, tokens: configuration.answerBudget.tokenLimit,
                        tokenField: Self.firstField(configuration.tokenField))
        let reply = try await ask(body)
        try Self.statusError(reply.status, body: reply.data, truncated: reply.truncated, key: configuration.apiKey)
        return try Self.text(of: reply)
    }

    /// **One request, with the token-field negotiation both callers need**: some servers (and older OpenAI models) refuse
    /// `max_completion_tokens`; a 400 or 422 is answered once with the older field, and only then. The retry is the
    /// status's, not the sentence's: DeepSeek answers 422 with "Invalid Parameters" and does not necessarily name the
    /// field (measured 2026-10-25). A probe uses the same path, so a server that answers questions does not fail its
    /// own connection test. **Only a retry that succeeds replaces the first reply**: one that fails too, by status or
    /// transport, would hide what the server said about the request the profile meant to send. A body with no token
    /// field has nothing to negotiate: the same request again would get the same refusal.
    private func ask(_ body: Body) async throws(AIProviderError) -> Reply {
        let reply = try await send(body)
        guard reply.status == 400 || reply.status == 422, let tokens = body.tokens else { return reply }
        do {
            let retry = try await send(Body(model: body.model, question: body.question, tokens: tokens,
                                            tokenField: body.tokenField == .completion ? .max : .completion))
            return (200..<300).contains(retry.status) ? retry : reply
        } catch {
            if error == .cancelled { throw error }
            return reply
        }
    }

    /// The field the profile prefers first; the retry covers the other.
    private static func firstField(_ preference: AIProviderTokenField) -> TokenField {
        preference == .completionFirst ? .completion : .max
    }

    /// **The connection test**: one token spent proving the address, the model and the key. Nothing runs unless this
    /// is called.
    public func probe() async throws(AIProviderError) -> Probe {
        let body = Body(model: configuration.model, question: "ping", tokens: Self.probeTokens,
                        tokenField: Self.firstField(configuration.tokenField))
        let clock = ContinuousClock()
        let start = clock.now
        let reply = try await ask(body)
        try Self.statusError(reply.status, body: reply.data, truncated: reply.truncated, key: configuration.apiKey)
        // A one-token probe normally ends at the token ceiling; that is success here, not truncation.
        _ = try Self.text(of: reply, allowEmpty: true, allowLength: true)
        return .ready(latency: clock.now - start)
    }

    // MARK: - The wire

    struct Reply: Sendable {
        let status: Int
        let data: Data
        /// The error-body ceiling cut this body: a message worded from it must not show a half-echoed key.
        let truncated: Bool
        /// A streamed 2xx, put together; `nil` for a body read whole.
        var streamed: StreamedAnswer? = nil
    }

    /// The answer in a 2xx reply, from its stream or its whole body.
    private static func text(of reply: Reply, allowEmpty: Bool = false,
                             allowLength: Bool = false) throws(AIProviderError) -> String {
        if let streamed = reply.streamed { return try streamed.text(allowEmpty: allowEmpty, allowLength: allowLength) }
        return try content(of: reply.data, allowEmpty: allowEmpty, allowLength: allowLength)
    }

    private enum TokenField: String, Sendable {
        case completion = "max_completion_tokens"
        case max = "max_tokens"
    }

    private struct Body: Sendable {
        let model: String
        let question: String
        /// `nil` sends no token field.
        let tokens: Int?
        let tokenField: TokenField

        func json(temperature: AIProviderTemperature, stream: Bool) throws(AIProviderError) -> Data {
            // No `stream_options`: some servers refuse a member they do not know.
            var object: [String: Any] = [
                "model": model,
                "messages": [["role": "user", "content": question]],
                "stream": stream,
            ]
            if let tokens { object[tokenField.rawValue] = tokens }
            if temperature == .zero { object["temperature"] = 0 }
            guard let data = try? JSONSerialization.data(withJSONObject: object) else { throw .badShape }
            return data
        }
    }

    /// The watchdog's signal that a limit passed: private, so no other failure can be mistaken for it.
    private struct Expired: Error {}

    /// **The configuration's timeouts hold on every session**: the transfer races a watchdog, and the first to finish
    /// cancels the other. URLSession's own limits apply only to the session it was given, and cannot say "no ceiling".
    /// A caller's cancel is `.cancelled` whichever child noticed it first; only the watchdog's expiry is `.timeout`.
    private func send(_ body: Body) async throws(AIProviderError) -> Reply {
        var request = URLRequest(url: configuration.endpoint.appending(path: "chat/completions"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let streamed = configuration.delivery == .streamed
        if streamed { request.setValue("text/event-stream", forHTTPHeaderField: "Accept") }
        Wire.authorize(&request, auth: configuration.auth, key: configuration.apiKey)
        request.httpBody = try body.json(temperature: configuration.temperature, stream: streamed)
        let start = ContinuousClock.now
        let lastByte = Locked(start)
        do {
            return try await withThrowingTaskGroup(of: Reply.self) { group in
                group.addTask { [request] in try await transfer(request, lastByte: lastByte) }
                group.addTask {
                    try await Self.expiry(of: lastByte, start: start, timeouts: configuration.timeouts)
                    throw Expired()
                }
                defer { group.cancelAll() }
                guard let reply = try await group.next() else { throw AIProviderError.unreachable }
                return reply
            }
        } catch {
            if Task.isCancelled { throw .cancelled }
            switch error {
            case is Expired: throw .timeout
            case let error as AIProviderError: throw error
            case is CancellationError: throw .cancelled
            default: throw .unreachable
            }
        }
    }

    /// Returns once `idle` has passed since the last byte, or `ceiling` since `start`; throws only when cancelled.
    private static func expiry(of lastByte: Locked<ContinuousClock.Instant>, start: ContinuousClock.Instant,
                               timeouts: Timeouts) async throws {
        let ceiling = timeouts.ceiling.map { start + $0 }
        while true {
            var deadline = lastByte.withLock { $0 } + timeouts.idle
            if let ceiling { deadline = min(deadline, ceiling) }
            if ContinuousClock.now >= deadline { return }
            try await Task.sleep(until: deadline, clock: .continuous)
        }
    }

    /// One request and the body its status calls for; every byte received, headers included, is noted in `lastByte`.
    private func transfer(_ request: URLRequest,
                          lastByte: Locked<ContinuousClock.Instant>) async throws(AIProviderError) -> Reply {
        let delegate = RedirectGuard(origin: origin)
        let touch = { lastByte.withLock { $0 = .now } }
        do {
            let (bytes, response) = try await session.bytes(for: request, delegate: delegate)
            touch()
            guard let http = response as? HTTPURLResponse else { throw AIProviderError.badShape }
            if delegate.refused { throw AIProviderError.redirectRefused }
            guard origin == Origin(http.url ?? configuration.endpoint) else { throw AIProviderError.redirectRefused }
            // **The status decides what body is wanted, and whether one is wanted at all**: a status this library words
            // from its code alone is answered from its headers and its transfer cancelled, so a body behind it — huge,
            // broken or slow — cannot mask it. Only a 2xx is read whole, under the answer ceiling. (A response whose
            // body never *starts* is indistinguishable from an unanswered request in URLSession; that one is still the
            // timeout it was, and the deadline is truthful.)
            let status = http.statusCode
            if (200..<300).contains(status) {
                if configuration.delivery == .streamed, !Self.isJSON(http) {
                    let answer = try await stream(bytes, status: status, onByte: touch)
                    return Reply(status: status, data: Data(), truncated: false, streamed: answer)
                }
                let body = try await Wire.collect(bytes, cap: configuration.responseCap, onByte: touch)
                return Reply(status: status, data: body.data, truncated: body.truncated)
            }
            if Self.needsNoBody(status) {
                bytes.task.cancel()
                return Reply(status: status, data: Data(), truncated: false)
            }
            // **A broken body keeps the status**: the server did answer, with a status this library reads; its
            // sentence is lost, its verdict is not. A cancellation is the caller's and is never swallowed.
            guard let body = await Wire.collectForMessage(bytes, cap: Self.errorBodyCap, onByte: touch) else {
                throw AIProviderError.cancelled
            }
            return Reply(status: status, data: body.data, truncated: body.truncated)
        } catch let error as AIProviderError {
            throw error
        } catch let failure as WireFailure {
            switch failure {
            case .tooLarge, .unexpected: throw .badShape
            case .cancelled: throw .cancelled
            case .transport(let code): throw Self.transport(code)
            }
        } catch let error as URLError {
            throw Self.transport(error.code)
        } catch is CancellationError {
            throw .cancelled
        } catch {
            throw .unreachable
        }
    }

    /// **A stream read until its answer ends**: every byte, a comment's included, is activity for the idle limit, and
    /// once the answer has ended the rest of the transfer is cancelled, not read.
    private func stream(_ bytes: URLSession.AsyncBytes, status: Int, onByte: () -> Void) async throws -> StreamedAnswer {
        var events = ServerSentEvents(cap: configuration.responseCap)
        var answer = StreamedAnswer(status: status, cap: configuration.responseCap, key: configuration.apiKey)
        for try await byte in bytes {
            onByte()
            guard let event = try events.take(byte), try answer.take(event) else { continue }
            bytes.task.cancel()
            return answer
        }
        if let event = try events.finish() { _ = try answer.take(event) }
        return answer
    }

    /// A streamed request answered with JSON was not streamed: the server ignored `stream: true`. Its media type is
    /// compared without parameters; a reply with no type is read as a stream.
    private static func isJSON(_ response: HTTPURLResponse) -> Bool {
        let type = response.value(forHTTPHeaderField: "Content-Type")?.split(separator: ";").first
        return type?.trimmingCharacters(in: .whitespaces).lowercased() == "application/json"
    }

    static func transport(_ code: URLError.Code) -> AIProviderError {
        switch code {
        case .cancelled: .cancelled
        case .timedOut: .timeout
        default: .unreachable
        }
    }

    /// **A refusal says what the server said** (measured 2026-10-25): a status this library has no typed meaning for —
    /// a cloud's 400, 402 or 422 — carries the server's own sentence to the reader. `.badShape` is kept for what it
    /// names: a 2xx whose body is not the contract's shape.
    static func statusError(_ status: Int, body: Data, truncated: Bool = false, key: String?) throws(AIProviderError) {
        switch status {
        case 200..<300: return
        case 401, 403: throw .unauthorised
        case 404: throw .modelNotFound
        case 429: throw .rateLimited
        case 300..<400: throw .redirectRefused
        default: throw .server(status: status, message: message(in: body, truncated: truncated, key: key))
        }
    }

    /// A status this library words from its code alone: its body is neither read nor needed.
    static func needsNoBody(_ status: Int) -> Bool {
        switch status {
        case 401, 403, 404, 429, 300..<400: true
        default: false
        }
    }

    /// The server's words, bounded: an OpenAI-compatible `error.message`, the raw body when it is not JSON, whitespace
    /// collapsed, 300 characters at most, and never the key even if a broken server echoes it back.
    static func message(in body: Data, truncated: Bool = false, key: String?) -> String {
        var text = ""
        if let object = try? JSONSerialization.jsonObject(with: body) as? [String: Any] {
            if let error = object["error"] as? [String: Any], let message = error["message"] as? String {
                text = message
            } else if let error = object["error"] as? String {
                text = error
            }
        } else {
            text = String(decoding: body, as: UTF8.self)
        }
        // **Redact before shortening**: a key that straddles the 300-character cut would otherwise keep its first
        // characters in the message.
        if let key, !key.isEmpty { text = text.replacingOccurrences(of: key, with: "…") }
        text = text.split(whereSeparator: \.isWhitespace).joined(separator: " ")
        if truncated, let key, !key.isEmpty {
            // **A body cut mid-key holds a proper prefix of it, which no exact-match redaction can find.** Whitespace
            // collapsing can bring that fragment into the visible 300 characters, so a cut body loses a trailing
            // partial key. A server message ending in the key's first characters is worth less than those characters.
            for length in stride(from: min(key.count - 1, text.count), through: 1, by: -1)
            where text.hasSuffix(String(key.prefix(length))) {
                text.removeLast(length)
                break
            }
        }
        return String(text.prefix(300))
    }

    /// The answer text in a chat-completions reply; `.truncated` when the server says it stopped at the token ceiling,
    /// `.refused` when the model or a filter declined, `.badShape` for anything that is not one usable answer.
    static func content(of data: Data, allowEmpty: Bool = false, allowLength: Bool = false) throws(AIProviderError) -> String {
        guard let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw .badShape }
        // **The envelope is required even for a probe**: a 200 carrying `{}` proves nothing about the address or the
        // model. Only the content itself may be empty for a probe.
        guard let choices = object["choices"] as? [[String: Any]], let first = choices.first else { throw .badShape }
        // **Only a turn the server says it finished is an answer**: a missing, null or unknown reason, or a tool-call
        // turn, is not one, whatever content it carries.
        switch first["finish_reason"] as? String {
        case "stop": break
        case "length": if !allowLength { throw .truncated }
        case "content_filter": throw .refused
        default: throw .badShape
        }
        guard let message = first["message"] as? [String: Any] else { throw .badShape }
        if let refusal = message["refusal"] as? String, !refusal.isEmpty { throw .refused }
        let content: String
        if let text = message["content"] as? String {
            content = text
        } else if message["content"] is NSNull, allowEmpty {
            // **A reasoning model's one-token turn has no content yet**: the envelope is still the proof a probe exists
            // for (DeepSeek, 2026-10-25). An answer with no content is still no answer.
            content = ""
        } else {
            throw .badShape
        }
        if content.isEmpty, !allowEmpty { throw .badShape }
        return content
    }

    /// **Same origin or refused.** The guard is a task delegate so the `Authorization` header is never replayed to
    /// another host: the default behaviour would follow a redirect and carry the key with it.
    final class RedirectGuard: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
        let origin: Origin
        private let refusedFlag = Locked(false)
        var refused: Bool { refusedFlag.withLock { $0 } }

        init(origin: Origin) {
            self.origin = origin
        }

        func urlSession(
            _ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
            newRequest request: URLRequest
        ) async -> URLRequest? {
            guard origin == Origin(request.url) else {
                refusedFlag.withLock { $0 = true }
                return nil
            }
            return request
        }
    }
}

extension HTTPProvider: CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public var description: String { "HTTPProvider(\(id.kind.rawValue))" }
    public var debugDescription: String { description }
    public var customMirror: Mirror { Mirror(self, children: [:]) }
}

extension HTTPProvider.Configuration: CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public var description: String {
        "HTTPProvider.Configuration(\(AIProviderID.scrubbed(endpoint).absoluteString), model: \(model))"
    }
    public var debugDescription: String { description }
    public var customMirror: Mirror { Mirror(self, children: [:]) }
}

/// An endpoint's scheme, host and port: what must not change under a redirect.
struct Origin: Sendable, Equatable {
    let scheme: String
    let host: String
    let port: Int?

    init?(_ url: URL?) {
        guard let url, let scheme = url.scheme?.lowercased(), let host = url.host?.lowercased() else { return nil }
        self.scheme = scheme
        self.host = host
        self.port = url.port ?? (scheme == "https" ? 443 : scheme == "http" ? 80 : nil)
    }

    /// `https` anywhere; `http` only on loopback (the reader's own server), never on a network address.
    var isAcceptable: Bool {
        switch scheme {
        case "https": true
        case "http": Self.loopbackHosts.contains(host)
        default: false
        }
    }

    static let loopbackHosts: Set<String> = ["localhost", "127.0.0.1", "::1"]
}
