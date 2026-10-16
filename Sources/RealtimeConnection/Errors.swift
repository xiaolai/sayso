import Foundation

/// A failure class (spec/http-and-mint.md, classification). It decides retry and, in an app, the message shown. An
/// app may define further classes of its own; this package only ever returns these.
public enum ErrorClass: String, Sendable, Equatable, CaseIterable {
    case authExpired = "auth-expired"
    case authMode = "auth-mode"
    case codexError = "codex-error"
    case routeDenied = "route-denied"
    case invalidRequest = "invalid-request"
    case rateLimited = "rate-limited"
    case server
    case network
    case `protocol`
    case cancelled
    /// Programming errors only.
    case `internal`

    /// The class of an error, as Go's `openaihttp.Classify`: a ``ClassifiedError``'s own, found through an NSError's
    /// underlying errors too; cancelled for `CancellationError` and a cancelled URL load; network for an expired
    /// ``RealtimeDeadline`` and any other URL failure; internal for anything else; nil for no error.
    public static func of(_ error: Error?) -> ErrorClass? {
        guard let error else { return nil }
        var next: Error? = error
        // As CodexAuthError.state(of:): every object seen is kept until the walk ends, so a released bridging box
        // cannot free its address for the next one, and a chain that loops back ends there.
        var seen: [ObjectIdentifier: AnyObject] = [:]
        while let e = next {
            if let c = e as? ClassifiedError { return c.errorClass }
            if e is CancellationError { return .cancelled }
            if e is RealtimeDeadline.Expired { return .network }
            if let u = e as? URLError { return u.code == .cancelled ? .cancelled : .network }
            let underlying = (e as NSError).userInfo[NSUnderlyingErrorKey]
            if let object = underlying as AnyObject? {
                guard seen.updateValue(object, forKey: ObjectIdentifier(object)) == nil else { break }
            }
            next = underlying as? Error
        }
        return .internal
    }
}

/// An error that carries its class: ``RealtimeError``, and the CodexAuth errors. ``ErrorClass/of(_:)`` reads it.
public protocol ClassifiedError: Error {
    var errorClass: ErrorClass { get }
}

/// A string scrubbed whenever it is stored, so neither a field set after construction nor reflection (`dump`,
/// `Mirror`) can reach an unscrubbed value.
@propertyWrapper
public struct Scrubbed: Sendable, Equatable {
    private var value: String
    public init(wrappedValue: String) { value = Secret.scrub(wrappedValue) }
    public var wrappedValue: String {
        get { value }
        set { value = Secret.scrub(newValue) }
    }
}

/// A classified failure. `type`, `code`, `param` and `message` come from the provider's error body or event; every
/// string is scrubbed whenever it is stored, and ``description`` scrubs again. No header or bearer is ever stored.
/// `message` is for diagnostics, never for display.
public struct RealtimeError: ClassifiedError, Sendable, Equatable, CustomStringConvertible {
    public var errorClass: ErrorClass
    /// The HTTP status; 0 if none.
    public var status: Int
    @Scrubbed public var type: String
    /// The provider error code; a number is kept as its decimal string.
    @Scrubbed public var code: String
    @Scrubbed public var param: String
    @Scrubbed public var message: String
    /// `x-request-id`.
    @Scrubbed public var requestID: String
    /// The call a route-denied failure is tracked against; the caller chooses the names.
    @Scrubbed public var route: String
    /// Set for a realtime error the session cannot continue after (spec/session.md).
    public var fatal: Bool
    /// The underlying cause, scrubbed text only.
    @Scrubbed public var cause: String

    public init(
        _ errorClass: ErrorClass, status: Int = 0, type: String = "", code: String = "", param: String = "",
        message: String = "", requestID: String = "", route: String = "", fatal: Bool = false, cause: String = ""
    ) {
        self.errorClass = errorClass
        self.status = status
        self.type = type
        self.code = code
        self.param = param
        self.message = message
        self.requestID = requestID
        self.route = route
        self.fatal = fatal
        self.cause = cause
    }

    public var description: String {
        var s = "openai: " + errorClass.rawValue
        if !route.isEmpty { s += " (\(route))" }
        if status != 0 { s += ": HTTP \(status)" }
        if !code.isEmpty { s += " code=\(code)" }
        if !param.isEmpty { s += " param=\(param)" }
        if !message.isEmpty { s += ": " + message }
        if !cause.isEmpty { s += ": " + cause }
        if !requestID.isEmpty { s += " (request id \(requestID))" }
        return Secret.scrub(s)
    }

    /// The HTTP rows of the taxonomy.
    public static func classify(status: Int, code: String) -> ErrorClass {
        switch status {
        case 401 where code == "missing_scope": return .routeDenied
        case 401: return .authExpired
        case 403, 404: return .routeDenied
        case 429: return .rateLimited
        case 500...: return .server
        case 400...: return .invalidRequest
        default: return .protocol  // 1xx/3xx where a final 2xx was expected
        }
    }

    /// The class of a close code received from the peer. 1006 (abnormal closure) never travels on the wire: it stands
    /// for a dropped connection, which is a network failure.
    public static func classify(closeCode code: Int) -> ErrorClass {
        switch code {
        case 1006: return .network
        case 1011, 1012, 1013, 1014, 1001: return .server
        case 1008: return .invalidRequest
        case 1009, 1002, 1003, 1007, 1000, 1005: return .protocol  // the peer ended a session still expected to run
        default: return .server
        }
    }

    /// A WebSocket close received while more events were expected. The reason is scrubbed.
    public static func connectionClosed(code: Int, reason: String) -> RealtimeError {
        RealtimeError(
            classify(closeCode: code), code: String(code),
            message: "realtime connection closed: " + Secret.scrub(reason))
    }

    private static let fatalCodes: Set<String> = ["session_expired", "invalid_api_key", "session_not_found"]
    private static let fatalTypes: Set<String> = ["authentication_error", "permission_error"]

    /// A realtime `error` event, classified by its `error.type` and `error.code` and marked fatal when the session
    /// cannot continue. All strings are scrubbed.
    public static func errorEvent(type: String, code: String, param: String, message: String) -> RealtimeError {
        let c: ErrorClass
        if code == "rate_limit_exceeded" || code == "insufficient_quota" || type == "rate_limit_error"
            || type == "insufficient_quota"
        {
            c = .rateLimited
        } else if code == "missing_scope" {
            c = .routeDenied  // the caller sets route: it knows which session this was
        } else if type == "invalid_request_error" {
            c = .invalidRequest
        } else {
            c = .server  // server_error and anything a later API adds
        }
        return RealtimeError(
            c, type: Secret.scrub(type), code: Secret.scrub(code), param: Secret.scrub(param),
            message: Secret.scrub(message), fatal: fatalCodes.contains(code) || fatalTypes.contains(type))
    }
}
