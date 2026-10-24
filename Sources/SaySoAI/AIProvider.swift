import Foundation

/// Which source answers: a kind the app persists and shows, never a prompt or a key.
public enum AIProviderKind: String, Sendable, Equatable, CaseIterable {
    case appleOnDevice
    case http
    case claudeCLI
    case codexCLI
}

/// A provider's identity: its kind and, for an endpoint, the address it talks to. No display text, no secret.
public struct AIProviderID: Sendable, Equatable {
    public let kind: AIProviderKind
    public let endpoint: URL?

    public init(kind: AIProviderKind, endpoint: URL? = nil) {
        self.kind = kind
        // **An identity carries no credential**: a URL may embed a user, a password or a fragment, and the identity is
        // what diagnostics and logs print. The endpoint's path and query still tell two servers apart.
        self.endpoint = endpoint.flatMap { url in
            guard var parts = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
            parts.user = nil
            parts.password = nil
            parts.fragment = nil
            return parts.url ?? url
        }
    }
}

/// **What a provider is**: it takes the composed question and returns the model's raw answer text. Validation is the
/// caller's; nothing here interprets the answer, and nothing here falls back to another provider (no automatic chain).
public protocol AIProvider: Sendable {
    var id: AIProviderID { get }

    /// The raw answer text for a composed question. A cancelled ask throws `.cancelled`, never a network failure.
    func answer(to question: String) async throws(AIProviderError) -> String
}

/// **Why a provider did not answer.** Every case is typed and free of the question, the key and any endpoint
/// credential: a description is safe for a log line because there is nothing in it to scrub.
public enum AIProviderError: Error, Equatable, Sendable {
    /// An `http` endpoint that is not loopback, refused before anything is sent.
    case insecureEndpoint
    /// A redirect would leave the endpoint's origin (scheme, host and port); never followed, so the key never travels.
    case redirectRefused
    case unreachable
    case unauthorised
    case modelNotFound
    case rateLimited
    /// The model or the server declined to answer.
    case refused
    /// The answer was cut off (`finish_reason` of `length`): a partial answer is never a result.
    case truncated
    /// The response was not the shape the contract asks for.
    case badShape
    /// **The server refused the request with a status this library has no typed meaning for**, and its own words when
    /// it gave any: a cloud's 400, 402 or 422 — a missing model, an empty balance, an unsupported parameter — rather
    /// than a malformed reply. The message is the server's, bounded and scrubbed of the key; it is for the reader's
    /// eyes, never a log line (the code is what a log carries).
    case server(status: Int, message: String)
    case timeout
    case cancelled
    /// The CLI a source runs is not installed (or the explicit path does not resolve).
    case notInstalled
    /// The reader's CLI has no usable sign-in for this source.
    case notSignedIn
    /// The CLI is older than the version this source needs.
    case versionTooOld
    /// The CLI is signed in a way this source does not accept (an API key, or an unknown mode).
    case unsupportedAuth
    /// Apple's on-device model cannot run here, with the framework's own reason.
    case unavailable(AppleAvailability)
    /// The question did not fit the on-device model's context window.
    case contextExceeded
    /// The model's guardrails refused the text.
    case guardrail
    /// Any other framework failure, its message scrubbed of the question and bounded.
    case framework(String)

    /// A stable, display-free code the app can log and word itself.
    public var code: String {
        switch self {
        case .insecureEndpoint: "provider.insecureEndpoint"
        case .redirectRefused: "provider.redirectRefused"
        case .unreachable: "provider.unreachable"
        case .unauthorised: "provider.unauthorised"
        case .modelNotFound: "provider.modelNotFound"
        case .rateLimited: "provider.rateLimited"
        case .refused: "provider.refused"
        case .truncated: "provider.truncated"
        case .badShape: "provider.badShape"
        case .server(let status, _): "provider.server.\(status)"
        case .timeout: "provider.timeout"
        case .cancelled: "provider.cancelled"
        case .notInstalled: "provider.notInstalled"
        case .notSignedIn: "provider.notSignedIn"
        case .versionTooOld: "provider.versionTooOld"
        case .unsupportedAuth: "provider.unsupportedAuth"
        case .unavailable(let reason): "provider.unavailable.\(reason.code)"
        case .contextExceeded: "provider.contextExceeded"
        case .guardrail: "provider.guardrail"
        case .framework: "provider.framework"
        }
    }
}

extension AIProviderError: CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public var description: String { "AIProviderError(\(code))" }
    public var debugDescription: String { description }
    public var customMirror: Mirror { Mirror(self, children: [:]) }
}

/// Why Apple's on-device model cannot run, as a closed set of our own (the framework's enum is neither `Codable` nor
/// `CaseIterable`, and may gain a case).
public enum AppleAvailability: Sendable, Equatable {
    case available
    case deviceNotEligible
    case appleIntelligenceNotEnabled
    case modelNotReady
    /// **The system is older than the app's Apple source requires**: the version the source needs and the one the
    /// device runs. This is a reason of its own, never `.unknown`.
    case system(required: String, running: String)
    /// A reason this build does not know.
    case unknown

    /// **The cases without a payload**, for a row that must word each one. A `.system` value is infinitely variable
    /// (its versions are the reader's), so it is never in a list; `isKnownCase` covers it.
    public static let fixedCases: [AppleAvailability] = [
        .available, .deviceNotEligible, .appleIntelligenceNotEnabled, .modelNotReady, .unknown,
    ]

    /// Whether this value is one of the reasons this build knows.
    public var isKnownCase: Bool {
        switch self {
        case .available, .deviceNotEligible, .appleIntelligenceNotEnabled, .modelNotReady, .system, .unknown: true
        }
    }

    /// A stable, display-free code for logs and the provider error's identity (never a sentence).
    public var code: String {
        switch self {
        case .available: "available"
        case .deviceNotEligible: "deviceNotEligible"
        case .appleIntelligenceNotEnabled: "appleIntelligenceNotEnabled"
        case .modelNotReady: "modelNotReady"
        // A constant: the code is what logs carry, and a version in it would make a stable code depend on the system.
        case .system: "system"
        case .unknown: "unknown"
        }
    }
}
