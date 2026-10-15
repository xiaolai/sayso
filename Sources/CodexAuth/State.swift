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
public let minimumCodexVersion = "0.162.0"

/// A failure to obtain a usable token. `errorClass` is auth-expired for ``CodexAuthState/expired``, codex-error for
/// Codex and file failures, and auth-mode for the sign-in states. Every string is scrubbed; `message` and `cause`
/// are diagnostics, never for display.
public struct CodexAuthError: Error, Sendable, Equatable, CustomStringConvertible {
    public var state: CodexAuthState
    /// The reported sign-in mode (api-key, unsupported-auth), scrubbed and bounded; "" otherwise.
    public var mode: String
    /// "" when unknown.
    public var codexVersion: String
    public var errorClass: ErrorClass
    public var message: String
    public var cause: String

    public init(
        _ state: CodexAuthState, mode: String = "", codexVersion: String = "", message: String = "",
        cause: String = ""
    ) {
        self.state = state
        self.mode = mode
        self.codexVersion = codexVersion
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

    /// The state an error carries: ready for nil, codex-error for an error from elsewhere.
    public static func state(of error: Error?) -> CodexAuthState {
        guard let error else { return .ready }
        return (error as? CodexAuthError)?.state ?? .codexError
    }
}

let modeChatGPT = "chatgpt"
let modeAPIKey = "apikey"
private let maxModeLength = 64

/// nil for chatgpt and the state's error for any other mode; "" is not signed in.
func classifyMode(_ mode: String) -> CodexAuthError? {
    switch mode {
    case modeChatGPT: return nil
    case "": return CodexAuthError(.notSignedIn, message: "Codex is not signed in")
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
