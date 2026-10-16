import Foundation

/// Where a request carrying `Authorization` may go (spec/http-and-mint.md, origin policy): only
/// `https://api.openai.com` (and `wss://` to the same host for a session), port empty or 443, no userinfo, no `Host`
/// override. Anything else is refused before a connection is made, as class `internal`.
public struct OriginPolicy: Sendable {
    public static let apiHost = "api.openai.com"
    /// The only origin a bearer may be sent to.
    public static let apiBase = URL(string: "https://api.openai.com")!

    let testOrigins: Set<String>
    let realNetworkGuard: (@Sendable () -> String?)?

    /// The production policy. `realNetworkGuard`, when set, runs before every bearer request to the real origin; a
    /// non-nil reason refuses it as `internal`. An app uses it to stop unit tests from spending real usage.
    public init(realNetworkGuard: (@Sendable () -> String?)? = nil) {
        self.testOrigins = []
        self.realNetworkGuard = realNetworkGuard
    }

    /// A policy that also allows `host:port` test origins, over `http`/`ws` or `https`/`wss`. For tests only: each host
    /// must be loopback (`127.0.0.0/8`, `::1`) or `localhost`, so a misuse can never send a bearer off the machine.
    public init(testOrigins: [String], realNetworkGuard: (@Sendable () -> String?)? = nil) throws {
        var canonical: Set<String> = []
        for origin in testOrigins {
            let digits = origin.lastIndex(of: ":").map { origin[origin.index(after: $0)...] } ?? ""
            guard let colon = origin.lastIndex(of: ":"), !digits.isEmpty, digits.allSatisfy({ ("0"..."9").contains($0) }),
                let port = Int(digits), (1...65535).contains(port)
            else { throw RealtimeError(.internal, message: "test origin \(origin): want host:port") }
            let host = Self.unbracketed(String(origin[..<colon]))
            guard Self.isLoopback(host) else {
                throw RealtimeError(.internal, message: "test origin \(origin) is not a loopback host")
            }
            // Stored as check() spells them: the host bracketed only when it holds a colon, the port in decimal
            // without leading zeros, as URLComponents reports it.
            canonical.insert(Self.bracketed(host) + ":" + String(port))
        }
        self.testOrigins = canonical
        self.realNetworkGuard = realNetworkGuard
    }

    static func isLoopback(_ host: String) -> Bool {
        let h = host.lowercased()
        if h == "localhost" || h == "::1" { return true }
        let parts = h.split(separator: ".", omittingEmptySubsequences: false)
        return parts.count == 4 && parts[0] == "127" && parts.allSatisfy { p in UInt8(p) != nil }
    }

    /// Throws `internal` when `request` carries `Authorization` and its origin is not allowed. A request without
    /// `Authorization` passes unchecked.
    public func check(_ request: URLRequest) throws {
        guard let auth = request.value(forHTTPHeaderField: "Authorization"), !auth.isEmpty else { return }
        guard let url = request.url, let components = URLComponents(url: url, resolvingAgainstBaseURL: false) else {
            throw Self.refused("request without URL")
        }
        if components.user != nil || components.password != nil { throw Self.refused("userinfo in URL") }
        let host = Self.unbracketed(components.host ?? "")
        let hostPort = components.port.map { "\(Self.bracketed(host)):\($0)" } ?? Self.bracketed(host)
        if let override = request.value(forHTTPHeaderField: "Host"), override != hostPort {
            throw Self.refused("Host header override")
        }
        let scheme = components.scheme?.lowercased() ?? ""
        if testOrigins.contains(hostPort), ["http", "https", "ws", "wss"].contains(scheme) { return }
        guard scheme == "https" || scheme == "wss" else { throw Self.refused("scheme \"\(scheme)\"") }
        guard host == Self.apiHost else { throw Self.refused("host \"\(host)\"") }
        if let port = components.port, port != 443 { throw Self.refused("port \"\(port)\"") }
        if let realNetworkGuard, let why = realNetworkGuard() {
            throw Self.refused("real OpenAI origin over the real network refused: " + why)
        }
    }

    /// `URLComponents` returns an IPv6 host with or without brackets depending on the Foundation version.
    private static func unbracketed(_ host: String) -> String {
        host.hasPrefix("[") && host.hasSuffix("]") ? String(host.dropFirst().dropLast()) : host
    }

    private static func bracketed(_ host: String) -> String { host.contains(":") ? "[\(host)]" : host }

    /// A programming error: a bearer request was built for a forbidden origin.
    private static func refused(_ why: String) -> RealtimeError {
        RealtimeError(.internal, message: "origin policy: bearer request refused: " + Secret.scrub(why))
    }
}

/// A bounded reply to a request that succeeded with a 2xx status.
public struct HTTPReply: Sendable {
    public let status: Int
    public let headers: [String: String]
    public let body: Data

    public init(status: Int, headers: [String: String], body: Data) {
        self.status = status
        self.headers = headers
        self.body = body
    }
}

/// The client every bearer request and WebSocket session goes through: the origin policy, no redirects, the system
/// proxy settings (an app's environment), and the failure taxonomy. It has no overall timeout: every call takes one.
public final class HTTPClient: Sendable {
    public let policy: OriginPolicy
    let session: URLSession

    /// At most this much of an error body is read.
    public static let maximumErrorBody = 64 << 10

    /// `configuration` defaults to an ephemeral one: no cookies, cache or credential storage reach the disk.
    public init(policy: OriginPolicy = OriginPolicy(), configuration: URLSessionConfiguration = .ephemeral) {
        self.policy = policy
        let config = configuration.copy() as? URLSessionConfiguration ?? .ephemeral
        // Headers a configuration adds are invisible to the origin policy, which sees only the request: a bearer or a
        // Host override set there could reach any origin, so both are removed.
        if let extra = config.httpAdditionalHeaders {
            config.httpAdditionalHeaders = extra.filter { key, _ in
                guard let name = (key as? String)?.lowercased() else { return true }
                return name != "authorization" && name != "host"
            }
        }
        config.httpShouldSetCookies = false
        config.urlCache = nil
        // Stored credentials would let URLSession add Authorization after the origin policy saw the request.
        config.urlCredentialStorage = nil
        self.session = URLSession(configuration: config, delegate: RefuseRedirect(), delegateQueue: nil)
    }

    deinit { session.invalidateAndCancel() }

    /// Sends `request` within `timeout` seconds and returns the reply when its status is 2xx and its body is at most
    /// `limit` bytes. A non-2xx reply is a classified error (at most 64 KiB of its body read; `route` is kept only on a
    /// route-denied error); a longer body is `protocol`; a broken connection is `network`; the caller's cancellation is
    /// `cancelled`. Neither the request nor the body is ever logged.
    ///
    /// With `resendOnLostConnection`, a request whose connection was lost before any byte of a reply arrived
    /// (`URLError.networkConnectionLost`) is sent once more, within the same `timeout` (spec/http-and-mint.md, lost
    /// connection): the server may have closed a kept-alive connection as the request reached it, and URLSession does
    /// not resend a POST. Only a caller for whom a duplicate request is harmless asks for it; a body stream, which
    /// cannot be sent twice, is never resent.
    public func send(
        _ request: URLRequest, timeout: TimeInterval, limit: Int, route: String = "", resendOnLostConnection: Bool = false
    ) async throws -> HTTPReply {
        guard Deadline.nanoseconds(timeout) != nil, limit >= 0 else {
            throw RealtimeError(.internal, message: "HTTPClient.send needs a positive, finite timeout and a limit")
        }
        try policy.check(request)
        var request = request
        request.timeoutInterval = timeout
        let session = self.session
        let sending = request
        return try await withDeadline(seconds: timeout, what: "request") {
            let resend = resendOnLostConnection && sending.httpBodyStream == nil
            let (bytes, response) = try await Self.open(session, sending, resend: resend)
            guard let http = response as? HTTPURLResponse else {
                throw RealtimeError(.protocol, message: "not an HTTP response")
            }
            var headers: [String: String] = [:]
            for (k, v) in http.allHeaderFields {
                if let k = k as? String, let v = v as? String { headers[k.lowercased()] = v }
            }
            let ok = (200..<300).contains(http.statusCode)
            // The body is read into one buffer of the package's own, wiped as it grows and once released: a mint
            // reply holds the secret, and a refusal may name one. The bytes URLSession hands over are its own.
            guard ok else {
                // At most 64 KiB, and a body that fails to arrive still leaves the reply classified by its status.
                var body = BodyBuffer(declared: http.expectedContentLength, limit: Self.maximumErrorBody)
                do {
                    for try await byte in bytes {
                        body.append(byte)
                        if body.isFull { break }
                    }
                } catch {}
                bytes.task.cancel()
                throw Self.errorReply(
                    status: http.statusCode, requestID: headers["x-request-id"] ?? "", body: body.data, route: route)
            }
            var body = BodyBuffer(declared: http.expectedContentLength, limit: limit)
            var over = false
            for try await byte in bytes {
                if body.isFull {
                    over = true
                    break
                }
                body.append(byte)
            }
            if over {
                bytes.task.cancel()
                throw RealtimeError(
                    .protocol, status: http.statusCode, message: "reply exceeds \(limit) bytes",
                    requestID: headers["x-request-id"] ?? "")
            }
            return HTTPReply(status: http.statusCode, headers: headers, body: body.data)
        }
    }

    /// The reply's head and its body as it arrives; with `resend`, a connection lost before the head is tried once
    /// more.
    private static func open(_ session: URLSession, _ request: URLRequest, resend: Bool) async throws
        -> (URLSession.AsyncBytes, URLResponse)
    {
        do {
            return try await session.bytes(for: request)
        } catch let lost as URLError where resend && lost.code == .networkConnectionLost && !Task.isCancelled {
            return try await session.bytes(for: request)
        }
    }

    /// The classified error for a non-2xx reply: the provider body `{"error": {"message", "type", "code", "param"}}`
    /// is read with the wire rules (spec/session.md, wire JSON) when it has that shape, every string scrubbed; a body
    /// that is not strict JSON, or has a field of the wrong type, yields no fields. The body is read where it lies.
    public static func errorReply(status: Int, requestID: String, body: Data, route: String) -> RealtimeError {
        let fields = body.withUnsafeBytes { ProviderError($0) } ?? ProviderError()
        let errorClass = RealtimeError.classify(status: status, code: fields.code)
        return RealtimeError(
            errorClass, status: status, type: fields.type, code: fields.code, param: fields.param,
            message: fields.message, requestID: requestID, route: errorClass == .routeDenied ? route : "")
    }
}

/// The provider error object, read in place (``WireJSON/Borrowed``); a field of the wrong type discards the whole
/// body. Each field is scrubbed where it lies, a string once decoded into a buffer that is wiped, so only scrubbed
/// text is made a `String`: a secret the body names leaves no copy behind.
struct ProviderError {
    var type = "", code = "", param = "", message = ""

    init() {}

    /// nil when `body` is not one strict JSON object or a field has the wrong type.
    init?(_ body: UnsafeRawBufferPointer) {
        guard let doc = WireJSON.Borrowed(body), doc.kind(doc.root) == UInt8(ascii: "{") else { return nil }
        func field(_ object: Range<Int>, _ name: String) -> Range<Int>? {  // absent or null is no field
            guard let v = doc.member(object, name), !doc.isNull(v) else { return nil }
            return v
        }
        guard let e = field(doc.root, "error") else { return }
        guard doc.kind(e) == UInt8(ascii: "{") else { return nil }
        let m = field(e, "message"), t = field(e, "type"), p = field(e, "param")
        for v in [m, t, p] { if let v, doc.kind(v) != UInt8(ascii: "\"") { return nil } }
        func scrubbed(_ v: Range<Int>) -> String {  // a string as itself, any other value as its exact JSON text
            doc.withString(v) { Secret.scrub(utf8: $0) } ?? Secret.scrub(utf8: doc.bytes(of: v))
        }
        message = m.map(scrubbed) ?? ""
        type = t.map(scrubbed) ?? ""
        param = p.map(scrubbed) ?? ""
        code = field(e, "code").map(scrubbed) ?? ""
    }
}

/// Refuses every redirect, same host or not, so a bearer never follows a 3xx: the 3xx reply itself is delivered, and
/// classified as `protocol`.
final class RefuseRedirect: NSObject, URLSessionTaskDelegate, Sendable {
    func urlSession(
        _ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
    ) { completionHandler(nil) }

    func urlSession(
        _ session: URLSession, task: URLSessionTask, didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) { answerChallenge(challenge, completionHandler) }
}

/// Server trust is left to the system; every other challenge (HTTP Basic, Digest, NTLM, client certificates) is
/// refused, so URLSession never adds a credential the origin policy did not see. The 401 itself is delivered.
func answerChallenge(
    _ challenge: URLAuthenticationChallenge,
    _ completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
) {
    if challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust {
        completionHandler(.performDefaultHandling, nil)
    } else {
        completionHandler(.rejectProtectionSpace, nil)
    }
}

/// The class of a failure that is not already classified: the caller's cancellation, or a broken connection. One the
/// classifier does not recognise is still a broken connection, never a programming error.
func transportError(_ error: Error, _ what: String) -> Error {
    if error is RealtimeError { return error }
    if Task.isCancelled || error is CancellationError { return RealtimeError(.cancelled, message: what + " cancelled") }
    if let u = error as? URLError {
        return RealtimeError(.network, message: what + " failed", cause: "URLError \(u.code.rawValue)")
    }
    let ns = error as NSError
    return RealtimeError(.network, message: what + " failed", cause: "\(ns.domain) \(ns.code)")
}

/// Runs `operation` and fails it as `network` when it has not finished within `seconds`; every failure is classified.
/// The deadline is an instant fixed at the start, and a result is accepted only when the operation produced it before
/// that instant, so a late timer cannot admit a late success. At the deadline the operation is cancelled and awaited:
/// if it then ends with a classified HTTP reply (a status whose body was cut short by the deadline), that reply wins
/// over the timeout, as it does in Go. The caller's own cancellation is `cancelled`, whatever the operation made of it.
func withDeadline<T: Sendable>(
    seconds: TimeInterval, what: String, _ operation: @escaping @Sendable () async throws -> T
) async throws -> T {
    let clock = ContinuousClock()
    let deadline = clock.now.advanced(by: Deadline.duration(seconds) ?? .zero)
    do {
        return try await withThrowingTaskGroup(of: DeadlineOutcome<T>.self) { group in
            group.addTask {
                let v = try await operation()
                return .value(v, inTime: clock.now < deadline)
            }
            group.addTask {
                try await Task.sleep(until: deadline, clock: clock)
                return .expired
            }
            defer { group.cancelAll() }
            let timedOut = RealtimeError(.network, message: "\(what) timed out after \(seconds) s")
            guard let first = try await group.next() else { throw CancellationError() }
            if case .value(let v, let inTime) = first {
                if inTime { return v }
                throw timedOut
            }
            group.cancelAll()
            do {
                // Only a success produced before the instant: the operation can finish between the timer and here.
                if case .value(let v, true)? = try await group.next() { return v }
            } catch let e as RealtimeError where e.status != 0 {
                throw e
            } catch {}
            throw timedOut
        }
    } catch {
        // The task group's cancellation never reaches this task, so a cancelled caller is told apart from the
        // deadline here: an error body cut short by the caller's cancellation must not become an HTTP refusal.
        if Task.isCancelled { throw RealtimeError(.cancelled, message: what + " cancelled") }
        throw transportError(error, what)
    }
}

/// The first child of ``withDeadline(seconds:what:_:)`` to finish.
private enum DeadlineOutcome<T: Sendable>: Sendable {
    case value(T, inTime: Bool)
    case expired
}

/// Seconds to a timer's nanoseconds, refusing what would trap the conversion.
enum Deadline {
    /// Nil unless `seconds` is positive, finite and fits `Int64` nanoseconds (about 292 years), so every caller can
    /// build a `Duration` or a `UInt64` sleep from it without trapping.
    static func nanoseconds(_ seconds: TimeInterval) -> UInt64? {
        guard seconds > 0, seconds.isFinite else { return nil }
        let ns = seconds * 1_000_000_000
        guard ns < 0x1p63 else { return nil }  // 2⁶³: every smaller Double converts to Int64
        return UInt64(ns)
    }

    /// The `Duration` of a validated `seconds`.
    static func duration(_ seconds: TimeInterval) -> Duration? {
        nanoseconds(seconds).map { .nanoseconds(Int64($0)) }
    }
}
