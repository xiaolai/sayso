// swift-tools-version: 6.0
import PackageDescription

let strict: [SwiftSetting] = [.enableExperimentalFeature("StrictConcurrency")]
// The AI providers are written in Swift 6's language mode — the concurrency checking these protocol types need. Typed
// throws themselves are a compiler feature, not a language-mode one. The earlier products keep the mode they shipped
// with until each chooses to move.
let strictSix: [SwiftSetting] = [.swiftLanguageMode(.v6)]

let package = Package(
    name: "SaySo",
    platforms: [.macOS(.v14), .iOS(.v17)],
    products: [
        // CodexAuth needs a local Codex, so its API exists on macOS only; the other three also build for iOS.
        .library(name: "CodexAuth", targets: ["CodexAuth"]),
        .library(name: "RealtimeConnection", targets: ["RealtimeConnection"]),
        .library(name: "SaidGuard", targets: ["SaidGuard"]),
        .library(name: "PCM16", targets: ["PCM16"]),
        // The AI providers: one OpenAI-compatible client, provider profiles and their model catalogue, the reader's
        // CLIs, and Apple's on-device model. SaySoAI and SaySoAIApple build for macOS and iOS; SaySoAICLI's API exists
        // on macOS only, as CodexAuth's does, because it runs child processes.
        .library(name: "SaySoAI", targets: ["SaySoAI"]),
        .library(name: "SaySoAICLI", targets: ["SaySoAICLI"]),
        .library(name: "SaySoAIApple", targets: ["SaySoAIApple"]),
    ],
    targets: [
        .target(name: "RealtimeConnection", swiftSettings: strict),
        .target(name: "CodexAuth", dependencies: ["RealtimeConnection"], swiftSettings: strict),
        .target(name: "SaidGuard", swiftSettings: strict),
        .target(name: "PCM16", swiftSettings: strict),
        .target(name: "SaySoAI", swiftSettings: strictSix),
        .target(name: "SaySoAICLI", dependencies: ["SaySoAI", "CodexAuth"], swiftSettings: strictSix),
        .target(name: "SaySoAIApple", dependencies: ["SaySoAI"], swiftSettings: strictSix),
        .testTarget(
            name: "ConformanceTests",
            dependencies: ["CodexAuth", "RealtimeConnection", "SaidGuard", "PCM16"],
            swiftSettings: strict
        ),
        .testTarget(name: "AIProviderTests", dependencies: ["SaySoAI"], swiftSettings: strictSix),
        .testTarget(name: "AIProviderAppleTests", dependencies: ["SaySoAI", "SaySoAIApple"], swiftSettings: strictSix),
        .testTarget(name: "AIProviderCLITests", dependencies: ["SaySoAICLI", "SaySoAI", "CodexAuth"],
                    swiftSettings: strictSix),
    ],
    swiftLanguageModes: [.v5]
)
