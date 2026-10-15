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
        for origin in testOrigins {
            guard let colon = origin.lastIndex(of: ":"), !origin[origin.index(after: colon)...].isEmpty,
                Int(origin[origin.index(after: colon)...]) != nil
            else { throw RealtimeError(.internal, message: "test origin \(origin): want host:port") }
            let host = Self.unbracketed(String(origin[..<colon]))
            guard Self.isLoopback(host) else {
                throw RealtimeError(.internal, message: "test origin \(origin) is not a loopback host")
            }
        }
        // Stored as check() spells them: the host bracketed only when it holds a colon.
        self.testOrigins = Set(
            testOrigins.map { o in
                let colon = o.lastIndex(of: ":")!
                return Self.bracketed(Self.unbracketed(String(o[..<colon]))) + String(o[colon...])
            })
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
        self.session = URLSession(configuration: config, delegate: RefuseRedirect(), delegateQueue: nil)
    }

    deinit { session.invalidateAndCancel() }

    /// Sends `request` within `timeout` seconds and returns the reply when its status is 2xx and its body is at most
    /// `limit` bytes. A non-2xx reply is a classified error (at most 64 KiB of its body read; `route` is kept only on a
    /// route-denied error); a longer body is `protocol`; a broken connection is `network`; the caller's cancellation is
    /// `cancelled`. Neither the request nor the body is ever logged.
    public func send(_ request: URLRequest, timeout: TimeInterval, limit: Int, route: String = "") async throws
        -> HTTPReply
    {
        guard Deadline.nanoseconds(timeout) != nil, limit >= 0 else {
            throw RealtimeError(.internal, message: "HTTPClient.send needs a positive, finite timeout and a limit")
        }
        try policy.check(request)
        var request = request
        request.timeoutInterval = timeout
        let session = self.session
        let sending = request
        return try await withDeadline(seconds: timeout, what: "request") {
            let (bytes, response) = try await session.bytes(for: sending)
            guard let http = response as? HTTPURLResponse else {
                throw RealtimeError(.protocol, message: "not an HTTP response")
            }
            var headers: [String: String] = [:]
            for (k, v) in http.allHeaderFields {
                if let k = k as? String, let v = v as? String { headers[k.lowercased()] = v }
            }
            let ok = (200..<300).contains(http.statusCode)
            guard ok else {
                // At most 64 KiB, and a body that fails to arrive still leaves the reply classified by its status.
                var body = Data()
                do {
                    for try await byte in bytes {
                        body.append(byte)
                        if body.count == Self.maximumErrorBody { break }
                    }
                } catch {}
                bytes.task.cancel()
                defer { body.resetBytes(in: 0..<body.count) }
                throw Self.errorReply(status: http.statusCode, requestID: headers["x-request-id"] ?? "", body: body, route: route)
            }
            var body = Data()
            var over = false
            for try await byte in bytes {
                if body.count == limit {
                    over = true
                    break
                }
                body.append(byte)
            }
            if over {
                bytes.task.cancel()
                body.resetBytes(in: 0..<body.count)
                throw RealtimeError(.protocol, status: http.statusCode, message: "reply exceeds \(limit) bytes")
            }
            return HTTPReply(status: http.statusCode, headers: headers, body: body)
        }
    }

    /// The classified error for a non-2xx reply: the provider body `{"error": {"message", "type", "code", "param"}}`
    /// is read with the wire rules (spec/session.md, wire JSON) when it has that shape, every string scrubbed; a body
    /// that is not strict JSON, or has a field of the wrong type, yields no fields.
    public static func errorReply(status: Int, requestID: String, body: Data, route: String) -> RealtimeError {
        var type = "", code = "", param = "", message = ""
        if let root = try? WireJSON.parse(body), let o = try? WireJSON.Object(root),
            let fields = try? ProviderError(o)
        {
            (type, code, param, message) = (fields.type, fields.code, fields.param, fields.message)
        }
        let scrubbedCode = Secret.scrub(code)
        let errorClass = RealtimeError.classify(status: status, code: scrubbedCode)
        return RealtimeError(
            errorClass, status: status, type: type, code: scrubbedCode, param: param, message: message,
            requestID: requestID, route: errorClass == .routeDenied ? route : "")
    }
}

/// The provider error object; a field of the wrong type discards the whole body.
struct ProviderError {
    var type = "", code = "", param = "", message = ""

    init(_ body: WireJSON.Object) throws {
        guard let e = try body.object("error") else { return }
        message = try e.string("message") ?? ""
        type = try e.string("type") ?? ""
        param = try e.string("param") ?? ""
        code = try e.code("code")
    }
}

/// Refuses every redirect, same host or not, so a bearer never follows a 3xx: the 3xx reply itself is delivered, and
/// classified as `protocol`.
final class RefuseRedirect: NSObject, URLSessionTaskDelegate, Sendable {
    func urlSession(
        _ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
    ) { completionHandler(nil) }
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
/// At the deadline the operation is cancelled and awaited: if it then ends with a classified HTTP reply (a status
/// whose body was cut short by the deadline), that reply wins over the timeout, as it does in Go.
func withDeadline<T: Sendable>(
    seconds: TimeInterval, what: String, _ operation: @escaping @Sendable () async throws -> T
) async throws -> T {
    do {
        return try await withThrowingTaskGroup(of: DeadlineOutcome<T>.self) { group in
            group.addTask { .value(try await operation()) }
            group.addTask {
                try await Task.sleep(nanoseconds: Deadline.nanoseconds(seconds) ?? 0)
                return .expired
            }
            defer { group.cancelAll() }
            guard let first = try await group.next() else { throw CancellationError() }
            if case .value(let v) = first { return v }
            group.cancelAll()
            let timedOut = RealtimeError(.network, message: "\(what) timed out after \(seconds) s")
            do {
                if case .value(let v)? = try await group.next() { return v }
            } catch let e as RealtimeError where e.status != 0 {
                throw e
            } catch {}
            throw timedOut
        }
    } catch {
        throw transportError(error, what)
    }
}

/// The first child of ``withDeadline(seconds:what:_:)`` to finish.
private enum DeadlineOutcome<T: Sendable>: Sendable {
    case value(T)
    case expired
}

/// Seconds to a timer's nanoseconds, refusing what would trap the conversion.
enum Deadline {
    /// Nil unless `seconds` is positive, finite and fits `UInt64` nanoseconds (about 584 years).
    static func nanoseconds(_ seconds: TimeInterval) -> UInt64? {
        guard seconds > 0, seconds.isFinite else { return nil }
        let ns = seconds * 1_000_000_000
        guard ns < 0x1p64 else { return nil }  // 2⁶⁴: every smaller Double converts
        return UInt64(ns)
    }
}
