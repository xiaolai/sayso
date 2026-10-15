// swift-tools-version: 5.9
import PackageDescription

let strict: [SwiftSetting] = [.enableExperimentalFeature("StrictConcurrency")]

let package = Package(
    name: "SaySo",
    platforms: [.macOS(.v14), .iOS(.v17)],
    products: [
        // CodexAuth needs a local Codex, so its API exists on macOS only; the other three also build for iOS.
        .library(name: "CodexAuth", targets: ["CodexAuth"]),
        .library(name: "RealtimeConnection", targets: ["RealtimeConnection"]),
        .library(name: "SaidGuard", targets: ["SaidGuard"]),
        .library(name: "PCM16", targets: ["PCM16"]),
    ],
    targets: [
        .target(name: "RealtimeConnection", swiftSettings: strict),
        .target(name: "CodexAuth", dependencies: ["RealtimeConnection"], swiftSettings: strict),
        .target(name: "SaidGuard", swiftSettings: strict),
        .target(name: "PCM16", swiftSettings: strict),
        .testTarget(
            name: "ConformanceTests",
            dependencies: ["CodexAuth", "RealtimeConnection", "SaidGuard", "PCM16"],
            swiftSettings: strict
        ),
    ]
)
