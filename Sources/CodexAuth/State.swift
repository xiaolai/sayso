#if os(macOS)
import Foundation
import RealtimeConnection

/// The outcome of a probe (spec/credentials.md). Exactly one applies. Apps map a state to their own text; this
/// package has no user-facing strings.
public enum CodexAuthState: String, Sendable, Equatable, CaseIterable {
    case ready
    case noCodex = "no-codex"
    case codexTooOld = "codex-too-old"
    case notSignedIn = "not-signed-in"
    case noSignInFile = "no-sign-in-file"
    case apiKey = "api-key"
    case unsupportedAuth = "unsupported-auth"
    case expired
    case codexError = "codex-error"
    case appServerAuthUnavailable = "app-server-auth-unavailable"
    case providerWithoutOpenAIAuth = "provider-without-openai-auth"
}

/// The oldest Codex supported.
public let minimumCodexVersion = "0.161.0"

/// A failure to obtain a usable token. `errorClass` is auth-expired for ``CodexAuthState/expired``, codex-error for
/// Codex and file failures, and auth-mode for the sign-in states. Every string is scrubbed; `message` and `cause`
/// are diagnostics, never for display.
public struct CodexAuthError: Error, Sendable, Equatable, CustomStringConvertible, CustomReflectable {
    // Read-only outside the package, and sanitised on every assignment inside it, so a field read or `dump(e)` never
    // shows what the initialiser would have removed.
    public internal(set) var state: CodexAuthState
    /// The reported sign-in mode (api-key, unsupported-auth), scrubbed and bounded; "" otherwise.
    public internal(set) var mode: String { didSet { mode = cleanMode(mode) } }
    /// "" when unknown.
    public internal(set) var codexVersion: String { didSet { codexVersion = Secret.scrub(codexVersion) } }
    public internal(set) var errorClass: ErrorClass
    public internal(set) var message: String { didSet { message = Secret.scrub(message) } }
    public internal(set) var cause: String { didSet { cause = Secret.scrub(cause) } }

    public init(
        _ state: CodexAuthState, mode: String = "", codexVersion: String = "", message: String = "",
        cause: String = ""
    ) {
        self.state = state
        self.mode = cleanMode(mode)  // printable, scrubbed and bounded, whoever builds the error
        self.codexVersion = Secret.scrub(codexVersion)
        switch state {
        case .expired: errorClass = .authExpired
        case .codexError, .noCodex, .codexTooOld, .appServerAuthUnavailable: errorClass = .codexError
        default: errorClass = .authMode
        }
        self.message = Secret.scrub(message)
        self.cause = Secret.scrub(cause)
    }

    /// Scrubbed whole when formatted, so a field set after construction, and a secret registered since, are removed
    /// too.
    public var description: String {
        var s = "codexauth: " + state.rawValue
        if !mode.isEmpty { s += " (mode \(mode))" }
        s += ": openai: \(errorClass.rawValue): \(message)"
        if !cause.isEmpty { s += ": " + cause }
        return Secret.scrub(s)
    }

    /// Reflection (`dump`, `Mirror`) shows the fields scrubbed when read, as ``description`` does: scrubbing on
    /// assignment cannot remove a secret registered after the error was built.
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "state": state, "mode": Secret.scrub(mode), "codexVersion": Secret.scrub(codexVersion),
                "errorClass": errorClass, "message": Secret.scrub(message), "cause": Secret.scrub(cause),
            ], displayStyle: .struct)
    }

    /// The state an error carries: ready for nil, codex-error for an error from elsewhere. A CodexAuthError wrapped
    /// as an NSError's underlying error counts, as Go's StateOf follows wrapping.
    /// The whole chain is followed; a chain that loops back to an error already seen ends there.
    public static func state(of error: Error?) -> CodexAuthState {
        var next = error
        // Every object seen is kept until the walk ends: an identifier alone would let a released temporary (the
        // box bridging gives a Swift value) free its address for the next one, which would then read as a loop.
        var seen: [ObjectIdentifier: AnyObject] = [:]
        while let e = next {
            if let a = e as? CodexAuthError { return a.state }
            let underlying = (e as NSError).userInfo[NSUnderlyingErrorKey]
            // The stored object's identity: a loop of NSErrors repeats objects, while bridging a Swift error makes a
            // new box each time, which is why the check is on what userInfo holds.
            if let object = underlying as AnyObject? {
                guard seen.updateValue(object, forKey: ObjectIdentifier(object)) == nil else { break }
            }
            next = underlying as? Error
        }
        return error == nil ? .ready : .codexError
    }
}

let modeChatGPT = "chatgpt"
let modeAPIKey = "apikey"
private let maxModeLength = 64

/// nil for chatgpt and the state's error for any other mode, "" included; a nil method (null or absent) is not signed
/// in.
func classifyMode(_ mode: String?) -> CodexAuthError? {
    guard let mode else { return CodexAuthError(.notSignedIn, message: "Codex is not signed in") }
    switch mode {
    case modeChatGPT: return nil
    case modeAPIKey: return CodexAuthError(.apiKey, mode: modeAPIKey, message: "Codex is signed in with an API key")
    default:
        return CodexAuthError(
            .unsupportedAuth, mode: cleanMode(mode), message: "Codex is signed in with an unsupported mode")
    }
}

/// A reported mode made safe to print: printable (Go's unicode.IsPrint), scrubbed, at most 64 characters. Filtering
/// comes first: removing a character could join the pieces of a secret ("e\u{0}k_…") into one the scrubber
/// recognises.
func cleanMode(_ m: String) -> String {
    var printable = String.UnicodeScalarView()
    for s in m.unicodeScalars where isPrintable(s) { printable.append(s) }
    return String(String(Secret.scrub(String(printable)).unicodeScalars.prefix(maxModeLength)))
}

private func isPrintable(_ s: Unicode.Scalar) -> Bool {
    if s == " " { return true }
    switch s.properties.generalCategory {
    case .uppercaseLetter, .lowercaseLetter, .titlecaseLetter, .modifierLetter, .otherLetter,
        .nonspacingMark, .spacingMark, .enclosingMark,
        .decimalNumber, .letterNumber, .otherNumber,
        .connectorPunctuation, .dashPunctuation, .openPunctuation, .closePunctuation, .initialPunctuation,
        .finalPunctuation, .otherPunctuation,
        .mathSymbol, .currencySymbol, .modifierSymbol, .otherSymbol:
        return true
    default:
        return false
    }
}
#endif
