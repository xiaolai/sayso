import Foundation
import SaySoAI
import Testing

#if canImport(FoundationModels)
import FoundationModels
@testable import SaySoAIApple

@available(macOS 26.4, iOS 26.4, *)
@Generable
struct ProbeAnswer: Sendable {
    var text: String
}

/// **Apple's on-device model as a provider**: availability is reported, not assumed; an unavailable model refuses
/// before a session exists; a real generation runs only when asked for by name (`SAYSO_AI_APPLE=1`). No test here turns
/// on a network or spends a quota. A test target inherits the package's deployment floor, so each test enters the
/// 26.4 API inside a runtime check.
struct AppleProviderTests {
    @available(macOS 26.4, iOS 26.4, *)
    static var provider: AppleOnDeviceProvider<ProbeAnswer> {
        AppleOnDeviceProvider(configuration: .init(encode: { answer in
            #"{"text":"\#(answer.text)"}"#
        }))
    }

    /// Each framework answer maps to the library's case of the same meaning.
    @Test func availabilityMapsEachFrameworkCase() {
        guard #available(macOS 26.4, iOS 26.4, *) else { return }
        typealias P = AppleOnDeviceProvider<ProbeAnswer>
        #expect(P.mapped(.available) == .available)
        #expect(P.mapped(.unavailable(.deviceNotEligible)) == .deviceNotEligible)
        #expect(P.mapped(.unavailable(.appleIntelligenceNotEnabled)) == .appleIntelligenceNotEnabled)
        #expect(P.mapped(.unavailable(.modelNotReady)) == .modelNotReady)
        #expect(P.mappedReason(.modelNotReady) == .modelNotReady)
        #expect(AppleAvailability.fixedCases == [
            .available, .deviceNotEligible, .appleIntelligenceNotEnabled, .modelNotReady, .unknown,
        ])
    }

    /// When Apple's model cannot run here, the provider says exactly why and never creates a session.
    @Test func anUnavailableModelIsATypedRefusal() async {
        guard #available(macOS 26.4, iOS 26.4, *) else { return }
        let reason = AppleOnDeviceProvider<ProbeAnswer>.availability()
        guard reason != .available else { return }  // this Mac's model runs: nothing to refuse
        await #expect(throws: AIProviderError.unavailable(reason)) { try await Self.provider.answer(to: "q") }
    }

    /// A real answer, only on the caller's word: `SAYSO_AI_APPLE=1 swift test --filter AppleProviderTests`.
    @Test func aRealGeneration() async throws {
        guard #available(macOS 26.4, iOS 26.4, *) else { return }
        guard AppleOnDeviceProvider<ProbeAnswer>.availability() == .available,
              ProcessInfo.processInfo.environment["SAYSO_AI_APPLE"] == "1" else { return }
        let text = try await Self.provider.answer(to: "Say the single word: pong")
        #expect(text.contains("pong"))
    }
}

#else

/// **A platform whose SDK has no FoundationModels**: the fallback provider exists, and says the model is unavailable.
struct AppleProviderTests {
    struct ProbeAnswer: Sendable {
        var text: String
    }

    @Test func aPlatformWithoutTheFrameworkSaysUnavailable() async {
        let provider = AppleOnDeviceProvider<ProbeAnswer>(configuration: .init(encode: { _ in "" }))
        #expect(AppleOnDeviceProvider<ProbeAnswer>.availability() == .unknown)
        await #expect(throws: AIProviderError.unavailable(.unknown)) { try await provider.answer(to: "q") }
    }
}

#endif
