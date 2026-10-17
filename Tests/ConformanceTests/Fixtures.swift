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
                byID[id] = try expandRepeats(object, repeatable[component] ?? [], id: id)
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

    /// The members a case may write as a repeat, per component (spec/fixtures.md).
    static let repeatable = ["said-guard": ["script", "transcript"]]
    /// Bounds the text a repeat stands for: 16 times the said guard's cap, room for a case past it.
    static let maxRepeatBytes = 1 << 20

    /// The case with each named member that is written as a repeat replaced by the string it stands for.
    static func expandRepeats(_ object: [String: Any], _ names: [String], id: String) throws -> [String: Any] {
        var out = object
        for name in names {
            guard let spec = object[name] as? [String: Any] else { continue }
            do { out[name] = try repeatText(spec) } catch { throw FixtureError("fixture \(id): \(name): \(error)") }
        }
        return out
    }

    /// The text `{"repeat", "times", "prefix", "suffix"}` stands for: `prefix`, `repeat` written `times` times, then
    /// `suffix`. Any other member, a missing, null or mistyped one, an empty `repeat`, `times` below 1 and a text over
    /// `maxRepeatBytes` are refused, so a mistyped case fails its suite rather than testing a different input.
    static func repeatText(_ spec: [String: Any]) throws -> String {
        let names = ["repeat", "times", "prefix", "suffix"]
        if let unknown = spec.keys.sorted().first(where: { !names.contains($0) }) {
            throw FixtureError("repeat: unknown member \(unknown)")
        }
        func string(_ name: String) throws -> String {
            guard let s = spec[name] as? String else { throw FixtureError("repeat: \(name) is missing or not a string") }
            return s
        }
        let unit = try string("repeat"), prefix = try string("prefix"), suffix = try string("suffix")
        // JSONSerialization delivers true and 2.0 as NSNumber too; only a whole number that is no bool is a count.
        guard let n = spec["times"] as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), !CFNumberIsFloatType(n) else {
            throw FixtureError("repeat: times is missing or not a whole number")
        }
        guard !unit.isEmpty else { throw FixtureError("repeat: repeat is empty") }
        let times = n.int64Value
        guard times >= 1 else { throw FixtureError("repeat: times \(n) is below 1") }
        // unit is not empty, so a times over the cap is over it in bytes too; checking it first keeps the product in range.
        guard times <= Int64(maxRepeatBytes), prefix.utf8.count + unit.utf8.count * Int(times) + suffix.utf8.count <= maxRepeatBytes
        else { throw FixtureError("repeat: the text is over \(maxRepeatBytes) bytes") }
        return prefix + String(repeating: unit, count: Int(times)) + suffix
    }

    /// The unsigned token a `{"$jwt": claims}` placeholder stands for (spec/fixtures.md).
    static func jwt(_ claims: [String: Any]) -> String {
        func encode(_ data: Data) -> String {
            data.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
                .replacingOccurrences(of: "=", with: "")
        }
        // As spec/fixtures.md builds it, and as Go and Python write it: compact, keys sorted, "/" not escaped.
        let body = (try? JSONSerialization.data(withJSONObject: claims, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data()
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

/// The longest a test waits for a signal that should come at once: ample for a loaded CI runner, and short enough
/// that a lost signal fails the test rather than holding the run until the job's timeout.
let signalWait: TimeInterval = 30

/// The longest a test waits for one fixture case's async work, the slowest of which runs for a few seconds.
let caseWait: TimeInterval = 60

extension DispatchSemaphore {
    /// Waits at most `seconds`; false when the signal did not come.
    func waited(_ seconds: TimeInterval = signalWait) -> Bool { wait(timeout: .now() + seconds) == .success }
}

/// Polls `condition` until it holds or `seconds` pass, for state with no signal of its own; whether it held.
func eventually(_ seconds: TimeInterval = signalWait, _ condition: () -> Bool) async -> Bool {
    let deadline = Date().addingTimeInterval(seconds)
    while !condition() {
        if Date() >= deadline { return false }
        try? await Task.sleep(nanoseconds: 1_000_000)
    }
    return true
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
