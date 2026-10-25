import Foundation
import SaySoAI

#if canImport(FoundationModels)
import FoundationModels

/// **Apple's on-device model as a provider**, guided into the caller's own generated answer type: one fresh session per
/// ask, temperature 0, and the answer encoded to the text the caller's validator reads. The library does not know what
/// an answer means; the caller supplies its `Generable` type and the encoder for it.
@available(macOS 26.4, iOS 26.4, *)
public struct AppleOnDeviceProvider<Answer: Generable & Sendable>: AIProvider {
    /// The shared configuration type, so both builds expose the same `.init(encode:)` under the same name.
    public typealias Configuration = AppleAnswerConfiguration<Answer>

    public let id = AIProviderID(kind: .appleOnDevice)
    private let configuration: Configuration

    public init(configuration: Configuration) {
        self.configuration = configuration
    }

    /// What Apple reports here and now, mapped to the library's closed set. The caller decides whether this system is
    /// new enough for its own Apple source — the floor and its sentence are the app's, not this library's.
    public static func availability() -> AppleAvailability {
        mapped(SystemLanguageModel.default.availability)
    }

    public func answer(to question: String) async throws(AIProviderError) -> String {
        guard !Task.isCancelled else { throw .cancelled }
        let model = SystemLanguageModel(guardrails: .permissiveContentTransformations)
        if case .unavailable(let reason) = model.availability {
            throw .unavailable(Self.mappedReason(reason))
        }
        // **The token preflight refuses only a certain failure**: a question that alone is past the window can never
        // fit. It counts neither the answer schema nor the output budget, so a question under the window proves
        // nothing, and a count that cannot be taken is no reason to refuse the ask; the framework's own context error,
        // mapped below, decides every other case.
        do {
            if try await model.tokenCount(for: question) > model.contextSize { throw AIProviderError.contextExceeded }
        } catch is CancellationError {
            throw .cancelled
        } catch let error as AIProviderError {
            throw error
        } catch {
            // Advisory only.
        }
        guard !Task.isCancelled else { throw .cancelled }
        // One session per ask: no history between questions.
        let session = LanguageModelSession(model: model)
        do {
            // **Guided generation**: the answer is generated into the caller's fields, not asked for as prose that
            // happens to be JSON (measured: free-form answers failed a strict shape check far more often).
            let response = try await session.respond(to: question, generating: Answer.self,
                                                     options: GenerationOptions(temperature: 0))
            guard !Task.isCancelled else { throw AIProviderError.cancelled }
            return configuration.encode(response.content)
        } catch is CancellationError {
            throw .cancelled
        } catch let error as AIProviderError {
            // A cancellation checked after the answer arrives is this library's own error, not a framework failure.
            throw error
        } catch {
            throw Self.mapped(error)
        }
    }

    // MARK: - Mapping

    static func mapped(_ availability: SystemLanguageModel.Availability) -> AppleAvailability {
        switch availability {
        case .available:
            return .available
        case .unavailable(let reason):
            return mappedReason(reason)
        @unknown default:
            return .unknown
        }
    }

    static func mappedReason(_ reason: SystemLanguageModel.Availability.UnavailableReason) -> AppleAvailability {
        switch reason {
        case .deviceNotEligible: return .deviceNotEligible
        case .appleIntelligenceNotEnabled: return .appleIntelligenceNotEnabled
        case .modelNotReady: return .modelNotReady
        @unknown default: return .unknown
        }
    }

    /// **A framework failure as this library's error.** Two enums say the same things: `GenerationError` from 26 and
    /// `LanguageModelError` from 27. A system before 27 throws the first and one past it the second, so both are read —
    /// a rate limit or a full window is typed on either, never flattened into a generic failure.
    ///
    /// `#available` is a runtime check and cannot hide a type the SDK lacks, so the second enum is also gated at
    /// compile time: it ships in the macOS 27 SDK, whose compiler is Swift 6.4, and an earlier SDK builds without it.
    static func mapped(_ error: any Error) -> AIProviderError {
        if let generation = error as? LanguageModelSession.GenerationError {
            switch generation {
            case .exceededContextWindowSize: return .contextExceeded
            case .rateLimited, .concurrentRequests: return .rateLimited
            case .guardrailViolation, .refusal: return .guardrail
            case .assetsUnavailable, .unsupportedGuide, .unsupportedLanguageOrLocale, .decodingFailure:
                return .framework(diagnostic)
            @unknown default: return .framework(diagnostic)
            }
        }
        #if compiler(>=6.4)
        if #available(macOS 27, iOS 27, *), let model = error as? LanguageModelError {
            switch model {
            case .contextSizeExceeded: return .contextExceeded
            case .rateLimited: return .rateLimited
            case .guardrailViolation, .refusal: return .guardrail
            default: return .framework(diagnostic)
            }
        }
        #endif
        return .framework(diagnostic)
    }

    /// **A fixed diagnostic, never the framework's sentence or even its type name**: the text may quote the reader's
    /// question, and cutting the question out catches only an exact copy; a type name is runtime data too. The error's
    /// `code` is what a log line carries.
    static var diagnostic: String { "generation" }
}

#else

/// **A platform whose SDK has no FoundationModels**: the type exists so a consumer written against the fuller build
/// still compiles, and every ask says the model is unavailable.
public struct AppleOnDeviceProvider<Answer: Sendable>: AIProvider {
    public typealias Configuration = AppleAnswerConfiguration<Answer>

    public let id = AIProviderID(kind: .appleOnDevice)

    /// The configuration is accepted so a consumer written against the fuller build compiles unchanged; this build
    /// never generates, and has nothing to encode.
    public init(configuration: Configuration) {}

    public static func availability() -> AppleAvailability {
        .unknown
    }

    public func answer(to question: String) async throws(AIProviderError) -> String {
        guard !Task.isCancelled else { throw .cancelled }
        throw .unavailable(.unknown)
    }
}

#endif

/// **The caller's encoder, shared by both builds**: how its generated answer becomes the JSON text every provider
/// returns. A type of its own rather than one nested in each build's provider, because the two providers are declared
/// conditionally and a nested type would have to be written twice.
public struct AppleAnswerConfiguration<Answer: Sendable>: Sendable {
    public let encode: @Sendable (Answer) -> String

    public init(encode: @escaping @Sendable (Answer) -> String) {
        self.encode = encode
    }
}
