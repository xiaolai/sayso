#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// A failure of the app-server child: it did not start, crashed, timed out, misbehaved, or answered a call with a
/// JSON-RPC error. `message` never quotes the child's output; `rpcMessage` is scrubbed and kept out of
/// `description`, because a misbehaving child may put a credential there.
public struct AppServerError: ClassifiedError, Sendable, Equatable, CustomStringConvertible, CustomReflectable {
    /// The JSON-RPC code of an error reply; nil for any other failure.
    public let rpcCode: Int?
    public let rpcMessage: String
    public let message: String
    /// codex-error for a failure of Codex, cancelled for a call the caller gave up, internal for a configuration the
    /// client refused, as in Go.
    public let errorClass: ErrorClass

    /// Both strings are scrubbed.
    public init(_ message: String, rpcCode: Int? = nil, rpcMessage: String = "", errorClass: ErrorClass = .codexError) {
        self.message = Secret.scrub(message)
        self.rpcCode = rpcCode
        self.rpcMessage = Secret.scrub(rpcMessage)
        self.errorClass = errorClass
    }

    /// The JSON-RPC code for an unknown method.
    public static let methodNotFound = -32601

    /// Scrubbed whole when formatted, so a secret registered since is removed too.
    public var description: String {
        Secret.scrub(rpcCode.map { "\(message): JSON-RPC error \($0)" } ?? message)
    }

    /// Reflection shows what `description` shows; `rpcMessage` stays out, as there.
    public var customMirror: Mirror {
        Mirror(
            self, children: ["rpcCode": rpcCode as Any, "message": Secret.scrub(message), "errorClass": errorClass],
            displayStyle: .struct)
    }
}

/// The getAuthStatus result. The token is registered with the scrubber and redacted by every formatting path,
/// reflection (`dump`, `Mirror`) included.
public struct AuthStatus: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible,
    CustomReflectable
{
    /// `authMethod`; nil when Codex reports null or nothing (not signed in). An empty string is a mode, and not
    /// chatgpt.
    public let method: String?
    /// `requiresOpenaiAuth`; false means a provider needing no OpenAI sign-in is in force.
    public let requiresOpenAIAuth: Bool
    /// The credential Codex sent, or "".
    public let token: String

    public init(method: String?, requiresOpenAIAuth: Bool, token: String) {
        Secret.register(token)
        self.method = method
        self.requiresOpenAIAuth = requiresOpenAIAuth
        self.token = token
    }

    /// By bytes, as Go compares them: Swift's `==` treats canonically equivalent strings as equal, and two different
    /// credentials can be (`tok-\u{E9}`, `tok-e\u{301}`).
    public static func == (a: AuthStatus, b: AuthStatus) -> Bool {
        let sameMethod: Bool
        switch (a.method, b.method) {
        case (nil, nil): sameMethod = true
        case let (x?, y?): sameMethod = x.utf8.elementsEqual(y.utf8)
        default: sameMethod = false
        }
        return sameMethod && a.requiresOpenAIAuth == b.requiresOpenAIAuth && a.token.utf8.elementsEqual(b.token.utf8)
    }

    public var description: String { "AuthStatus{method: \(Secret.scrub(method ?? ""))}" }
    public var debugDescription: String { description }
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "method": Secret.scrub(method ?? ""), "requiresOpenAIAuth": requiresOpenAIAuth, "token": Secret.redacted,
            ], displayStyle: .struct)
    }
}

/// A message from the child that is not a reply: a notification, or a server request, which the client has already
/// answered with an error (``isServerRequest``). Formatting and reflection show the kind, the method scrubbed and
/// escaped (so a control character in it cannot forge a log line or reach a terminal) and the size of ``params``,
/// never the params themselves: a notification may carry what the user said, or a credential.
public struct AppServerEvent: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible,
    CustomReflectable
{
    public let method: String
    /// The params member exactly as the child wrote it, strict JSON; empty when absent (a `"params": null` is
    /// `null`).
    public let params: Data
    /// Set for a server request (it had an id), which was refused before the event was queued.
    public let isServerRequest: Bool

    public init(method: String, params: Data, isServerRequest: Bool) {
        self.method = method
        self.params = params
        self.isServerRequest = isServerRequest
    }

    /// ``params`` decoded as a JSON object; nil when absent or not an object.
    public func paramsObject() -> [String: Any]? {
        guard !params.isEmpty else { return nil }
        return (try? JSONSerialization.jsonObject(with: params, options: [.fragmentsAllowed])) as? [String: Any]
    }

    /// By bytes, as Go compares them.
    public static func == (a: AppServerEvent, b: AppServerEvent) -> Bool {
        a.method.utf8.elementsEqual(b.method.utf8) && a.params == b.params && a.isServerRequest == b.isServerRequest
    }

    public var description: String {
        "AppServerEvent{\(isServerRequest ? "server request" : "notification") \(Self.quoted(Secret.scrub(method))), \(params.count) bytes}"
    }
    public var debugDescription: String { description }
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "method": Self.quoted(Secret.scrub(method)), "params": "\(params.count) bytes",
                "isServerRequest": isServerRequest,
            ], displayStyle: .struct)
    }

    /// `s` in double quotes, with a quote, a backslash and every scalar that is not printable (controls, format
    /// characters such as a bidirectional override, separators other than the space) escaped, as Go's strconv.Quote
    /// writes them.
    static func quoted(_ s: String) -> String {
        var out = "\""
        for u in s.unicodeScalars {
            switch u {
            case "\"": out += "\\\""
            case "\\": out += "\\\\"
            case "\u{07}": out += "\\a"
            case "\u{08}": out += "\\b"
            case "\u{0C}": out += "\\f"
            case "\n": out += "\\n"
            case "\r": out += "\\r"
            case "\t": out += "\\t"
            case "\u{0B}": out += "\\v"
            default:
                if u.value < 0x20 || u.value == 0x7F {
                    out += "\\x" + String(format: "%02x", u.value)
                    continue
                }
                switch u.properties.generalCategory {
                case .control, .format, .lineSeparator, .paragraphSeparator, .surrogate, .unassigned, .privateUse,
                    .spaceSeparator where u != " ":
                    out += u.value <= 0xFFFF
                        ? "\\u" + String(format: "%04x", u.value) : "\\U" + String(format: "%08x", u.value)
                default: out.unicodeScalars.append(u)
                }
            }
        }
        return out + "\""
    }
}
#endif
