// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "SaySo",
    platforms: [.macOS(.v14), .iOS(.v17)],
    targets: [
        .testTarget(name: "ConformanceTests")
    ]
)
