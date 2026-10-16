import Foundation
import SaidGuard
import XCTest

/// The shared fixture corpus, read from this checkout through its manifest (spec/parity.md). A missing manifest or
/// file fails the test; it is never an empty corpus.
enum Fixtures {
    static let root = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        .appendingPathComponent("fixtures")

    struct Case {
        let id: String
        let component: String
        let requires: [String]
        let object: [String: Any]
    }

    static func load(_ component: String) throws -> [Case] {
        let manifestData = try Data(contentsOf: root.appendingPathComponent("MANIFEST.json"))
        guard let manifest = try JSONSerialization.jsonObject(with: manifestData) as? [String: Any],
            let entries = manifest["fixtures"] as? [[String: Any]], let count = manifest["count"] as? Int,
            count > 0, count == entries.count
        else { throw FixtureError("the manifest is empty or its count is wrong") }
        let wanted = entries.filter { $0["component"] as? String == component }
        guard !wanted.isEmpty else { throw FixtureError("the manifest lists no \(component) fixtures") }
        var byID: [String: [String: Any]] = [:]
        for file in Set(wanted.compactMap { $0["file"] as? String }) {
            for object in try read(root.appendingPathComponent(file)) where object["component"] as? String == component {
                guard let id = object["id"] as? String, byID[id] == nil else { throw FixtureError("fixture \(object["id"] ?? "?") appears twice") }
                byID[id] = object
            }
        }
        guard byID.count == wanted.count else { throw FixtureError("\(component): files hold \(byID.count) fixtures, the manifest \(wanted.count)") }
        return try wanted.map { entry in
            guard let id = entry["id"] as? String, let object = byID[id] else { throw FixtureError("fixture \(entry["id"] ?? "?") is missing") }
            return Case(id: id, component: component, requires: entry["requires"] as? [String] ?? [], object: object)
        }
    }

    private static func read(_ url: URL) throws -> [[String: Any]] {
        let data = try Data(contentsOf: url)
        if url.pathExtension == "jsonl" {
            return try data.split(separator: UInt8(ascii: "\n")).filter { !$0.isEmpty }.map {
                guard let o = try JSONSerialization.jsonObject(with: Data($0)) as? [String: Any] else { throw FixtureError("\(url.lastPathComponent): a line is not an object") }
                return o
            }
        }
        guard let array = try JSONSerialization.jsonObject(with: data) as? [[String: Any]] else { throw FixtureError("\(url.lastPathComponent) is not an array") }
        return array
    }

    /// The unsigned token a `{"$jwt": claims}` placeholder stands for (spec/fixtures.md).
    static func jwt(_ claims: [String: Any]) -> String {
        func encode(_ data: Data) -> String {
            data.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
                .replacingOccurrences(of: "=", with: "")
        }
        let body = (try? JSONSerialization.data(withJSONObject: claims, options: [.sortedKeys])) ?? Data()
        return encode(Data(#"{"alg":"none","typ":"JWT"}"#.utf8)) + "." + encode(body) + "." + encode(Data("sig".utf8))
    }

    static func substitute(_ value: Any) -> Any {
        if let object = value as? [String: Any] {
            if object.count == 1, let claims = object["$jwt"] as? [String: Any] { return jwt(claims) }
            return object.mapValues(substitute)
        }
        if let array = value as? [Any] { return array.map(substitute) }
        return value
    }
}

struct FixtureError: Error, CustomStringConvertible {
    let description: String
    init(_ description: String) { self.description = description }
}

/// Outcomes for tools/parity.py, written to results-swift.json when SAYSO_RESULTS names a path.
final class Results: @unchecked Sendable {
    static let shared = Results()
    static let components = ["credentials", "mint", "session", "pcm16", "said-guard"]
    /// The languages the guard spells out, so the declaration cannot drift from the implementation.
    static let capabilities = saidGuardSpellOutLanguages.sorted().map { "spellout:" + $0 }
    private let lock = NSLock()
    private var outcomes: [String: String] = [:]

    func record(_ id: String, _ outcome: String) {
        lock.lock()
        defer { lock.unlock() }
        outcomes[id] = outcomes[id] == nil ? outcome : "fail"
        write()
    }

    private func write() {
        guard let path = ProcessInfo.processInfo.environment["SAYSO_RESULTS"], !path.isEmpty else { return }
        let doc: [String: Any] = ["implementation": "swift", "components": Self.components, "capabilities": Self.capabilities, "results": outcomes]
        if let data = try? JSONSerialization.data(withJSONObject: doc, options: [.sortedKeys, .prettyPrinted]) {
            try? data.write(to: URL(fileURLWithPath: path), options: .atomic)
        }
    }
}

extension XCTestCase {
    /// Runs every fixture of a component; `check` returns nil for a pass, "exempt", or the reason for a failure.
    func runFixtures(_ component: String, _ check: (Fixtures.Case) throws -> String?) throws {
        let cases = try Fixtures.load(component)
        var passed = 0
        for c in cases {
            let outcome: String
            do { outcome = try check(c) ?? "pass" } catch { outcome = "\(error)" }
            switch outcome {
            case "pass":
                passed += 1
                Results.shared.record(c.id, "pass")
            case "exempt":
                Results.shared.record(c.id, "exempt")
            default:
                Results.shared.record(c.id, "fail")
                XCTFail("\(c.id): \(outcome)")
            }
        }
        print("\(component): \(cases.count) fixtures, \(passed) passed")
    }
}
