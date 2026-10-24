import Foundation

/// **An OpenAI-compatible endpoint — a cloud one or a server the reader runs — as transport only.**
///
/// No locality tier and no locality claim: the endpoint is the reader's address and this library promises nothing about
/// what serves it. The key lives in this value, in memory, and never in an error, a log line or a description.
/// Requests run on one kept `URLSession`, at temperature 0, non-streaming.
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

        public init(endpoint: URL, model: String, apiKey: String? = nil,
                    auth: AIProviderAuth = .bearer, tokenField: AIProviderTokenField = .completionFirst) {
            self.endpoint = endpoint
            self.model = model
            self.apiKey = apiKey
            self.auth = auth
            self.tokenField = tokenField
        }
    }

    /// The most tokens one answer may take: a quoted sense and one sentence fit well inside it; a larger budget only
    /// invites a truncated, unusable reply.
    public static let answerTokens = 1_024
    /// The probe's budget: one token is enough to prove the endpoint, the model and the key.
    public static let probeTokens = 1
    /// Per-request and whole-request ceilings; a hung endpoint fails as `.timeout`, never as a hang.
    public static let requestTimeout: TimeInterval = 60
    /// The largest answer body accepted; an answer is far smaller, and anything past this is refused as a bad shape.
    public static let responseCap = 1 << 20
    /// How much of a **refusal's** body is read for its sentence. A hostile server cannot turn a 401 into a bad shape by
    /// sending megabytes with it.
    public static let errorBodyCap = 64 << 10
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
    /// **An injected `session` owns its own timeouts**: the ceilings above are set only on the session this initializer
    /// creates. A caller that passes one (a test stand-in, a shared session) chose its limits, and this type does not
    /// silently re-time it.
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
            config.timeoutIntervalForRequest = Self.requestTimeout
            config.timeoutIntervalForResource = Self.resourceTimeout
            self.session = URLSession(configuration: config)
        }
    }

    public func answer(to question: String) async throws(AIProviderError) -> String {
        let body = Body(model: configuration.model, question: question, tokens: Self.answerTokens,
                        tokenField: Self.firstField(configuration.tokenField))
        let reply = try await ask(body)
        try Self.statusError(reply.status, body: reply.data, truncated: reply.truncated, key: configuration.apiKey)
        return try Self.content(of: reply.data)
    }

    /// **One request, with the token-field negotiation both callers need**: some servers (and older OpenAI models) refuse
    /// `max_completion_tokens`; a 400 or 422 is answered once with the older field, and only then. The retry is the
    /// status's, not the sentence's: DeepSeek answers 422 with "Invalid Parameters" and does not necessarily name the
    /// field (measured 2026-10-25). A probe uses the same path, so a server that answers questions does not fail its
    /// own connection test.
    private func ask(_ body: Body) async throws(AIProviderError) -> Reply {
        var reply = try await send(body)
        if reply.status == 400 || reply.status == 422 {
            reply = try await send(Body(model: body.model, question: body.question, tokens: body.tokens,
                                        tokenField: body.tokenField == .completion ? .max : .completion))
        }
        return reply
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
        _ = try Self.content(of: reply.data, allowEmpty: true, allowLength: true)
        return .ready(latency: clock.now - start)
    }

    // MARK: - The wire

    struct Reply: Sendable {
        let status: Int
        let data: Data
        /// The error-body ceiling cut this body: a message worded from it must not show a half-echoed key.
        let truncated: Bool
    }

    private enum TokenField: String, Sendable {
        case completion = "max_completion_tokens"
        case max = "max_tokens"
    }

    private struct Body: Sendable {
        let model: String
        let question: String
        let tokens: Int
        let tokenField: TokenField

        func json() throws(AIProviderError) -> Data {
            let object: [String: Any] = [
                "model": model,
                "messages": [["role": "user", "content": question]],
                "temperature": 0,
                "stream": false,
                tokenField.rawValue: tokens,
            ]
            guard let data = try? JSONSerialization.data(withJSONObject: object) else { throw .badShape }
            return data
        }
    }

    private func send(_ body: Body) async throws(AIProviderError) -> Reply {
        var request = URLRequest(url: configuration.endpoint.appending(path: "chat/completions"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        Wire.authorize(&request, auth: configuration.auth, key: configuration.apiKey)
        request.httpBody = try body.json()
        let delegate = RedirectGuard(origin: origin)
        do {
            let (bytes, response) = try await session.bytes(for: request, delegate: delegate)
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
                let body = try await Wire.collect(bytes, cap: Self.responseCap)
                return Reply(status: status, data: body.data, truncated: body.truncated)
            }
            if Self.needsNoBody(status) {
                bytes.task.cancel()
                return Reply(status: status, data: Data(), truncated: false)
            }
            // **A broken body keeps the status**: the server did answer, with a status this library reads; its
            // sentence is lost, its verdict is not. A cancellation is the caller's and is never swallowed.
            guard let body = await Wire.collectForMessage(bytes, cap: Self.errorBodyCap) else {
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
        if let finish = first["finish_reason"] as? String {
            if finish == "length", !allowLength { throw .truncated }
            if finish == "content_filter" { throw .refused }
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
    public var description: String { "HTTPProvider.Configuration(\(endpoint.absoluteString), model: \(model))" }
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
