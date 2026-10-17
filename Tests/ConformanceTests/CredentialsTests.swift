#if os(macOS)
import CodexAuth
import Darwin
import Foundation
import RealtimeConnection
import XCTest

/// The credentials fixtures (spec/fixtures.md), run against the scripted app-server, mirroring
/// go/conformance/credentials_test.go. Failure messages name keys and counts, never an environment value or a record
/// line: a spawn record holds the child's whole environment.
final class CredentialsTests: XCTestCase {
    func testCredentials() throws {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"), "the credentials fixtures need python3")
        let fake = Fixtures.root.deletingLastPathComponent().appendingPathComponent("tools/fake-app-server.py").path
        XCTAssertTrue(FileManager.default.fileExists(atPath: fake), "the fake app-server is missing")
        try runFixtures("credentials") { c in
            let unknown = CredentialWorld.unknownMembers(c.object)
            guard unknown.isEmpty else { return unknown.joined(separator: "; ") }
            let setup = c.object["setup"] as? [String: Any] ?? [:]
            if let privateSignIn = setup["privateSignIn"] as? [String: Any] {
                let problems = try PrivateSignInCase.run(
                    privateSignIn, now: CredentialWorld.int(setup["now"], 1_800_000_000),
                    want: c.object["expect"] as? [String: Any] ?? [:])
                return problems.isEmpty ? nil : problems.joined(separator: "; ")
            }
            let world = try CredentialWorld(setup, python: python, fake: fake)
            defer { world.cleanUp() }
            let problems: [String]
            if let client = setup["client"] as? [String: Any] {
                problems = try world.client(client, want: c.object["expect"] as? [String: Any] ?? [:])
            } else if let steps = c.object["steps"] as? [[String: Any]] {
                problems = try world.timeline(steps)
            } else {
                problems = try world.probe(c.object["expect"] as? [String: Any] ?? [:])
            }
            return problems.isEmpty ? nil : problems.joined(separator: "; ")
        }
    }
}

/// A value shared with `@Sendable` closures the configuration holds.
final class Box<T>: @unchecked Sendable {
    private let lock = NSLock()
    private var stored: T
    init(_ value: T) { stored = value }
    var value: T {
        get { lock.withLock { stored } }
        set { lock.withLock { stored = newValue } }
    }
}

/// Runs async work from a synchronous test body; `cancelWhen`, polled until the work ends, cancels it once it holds,
/// and `cancelledAt` records when. Work still running after ``caseWait`` is cancelled and fails the case.
func blocking<T: Sendable>(
    cancelWhen: (@Sendable () -> Bool)? = nil, cancelledAt: Box<Date?>? = nil,
    _ body: @escaping @Sendable () async throws -> T
) throws -> T {
    let done = DispatchSemaphore(value: 0)
    let result = Box<Result<T, Error>?>(nil)
    let finished = Box(false)
    let task = Task {
        do { result.value = .success(try await body()) } catch { result.value = .failure(error) }
        finished.value = true
        done.signal()
    }
    if let cancelWhen {
        // Cancelled on an event, not after a delay: a delay races the child's start, which a loaded machine slows.
        DispatchQueue.global().async {
            while !finished.value {
                if cancelWhen() {
                    cancelledAt?.value = Date()
                    task.cancel()
                    return
                }
                usleep(5_000)
            }
        }
    }
    guard done.waited(caseWait) else {
        task.cancel()
        throw FixtureError("the case's async work did not finish within \(Int(caseWait)) s")
    }
    return try result.value!.get()
}

/// One case's temporary machine: a home folder, the codex installation and the fake's record.
final class CredentialWorld {
    /// The case members, setup keys, file, client and step members this harness reads (spec/fixtures.md), as
    /// problems for any other; the expectation keys are checked as they are read.
    static func unknownMembers(_ c: [String: Any]) -> [String] {
        let setup = c["setup"] as? [String: Any]
        var p = FakeOpenAI.unknownKeys("case", c, FakeOpenAI.caseHead.union(["setup", "expect", "steps"]))
        p += FakeOpenAI.unknownKeys(
            "setup", setup,
            [
                "now", "marginSeconds", "source", "codex", "inheritedPath", "inheritedEnv", "codexHome", "file",
                "retry", "timeoutMs", "cancelOnRequest", "server", "client", "privateSignIn",
            ])
        p += FakeOpenAI.unknownKeys("setup.privateSignIn", setup?["privateSignIn"] as? [String: Any], ["token", "folder"])
        for name in ["file", "retry"] {
            p += FakeOpenAI.unknownKeys(
                "setup.\(name)", setup?[name] as? [String: Any], ["kind", "json", "raw", "hex", "rawRepeat", "padTo"])
        }
        let client = setup?["client"] as? [String: Any]
        p += FakeOpenAI.unknownKeys("setup.client", client, ["handler", "dir", "experimentalAPI", "calls"])
        for (i, call) in (client?["calls"] as? [[String: Any]] ?? []).enumerated() {
            p += FakeOpenAI.unknownKeys("setup.client.calls[\(i)]", call, ["method", "params", "typed"])
        }
        for (i, step) in (c["steps"] as? [[String: Any]] ?? []).enumerated() {
            p += FakeOpenAI.unknownKeys("steps[\(i)]", step, ["at", "op", "stale", "expect", "note"])
        }
        return p
    }

    let root: String, home: String, record: String
    private(set) var binDir: String
    private(set) var codexHome: String
    let now: Box<Date>
    let logs = Box<[String]>([])
    private(set) var cfg: CodexAuthConfiguration
    private let setup: [String: Any]

    init(_ s: [String: Any], python: String, fake: String) throws {
        setup = s
        let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("sayso-cred-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
        guard let real = realpath(tmp.path, nil) else { throw FixtureError("the temporary folder does not resolve") }
        root = String(cString: real)
        free(real)
        home = root + "/home"
        binDir = root + "/bin"
        record = root + "/record.jsonl"
        now = Box(Date(timeIntervalSince1970: TimeInterval(Self.int(s["now"], 1_800_000_000))))
        for d in [home, binDir] { try Self.mkdir(d) }

        let scenario = root + "/scenario.json"
        let server = s["server"].flatMap { $0 is NSNull ? nil : $0 } ?? [String: Any]()
        try JSONSerialization.data(withJSONObject: server, options: [.sortedKeys]).write(to: URL(fileURLWithPath: scenario))

        let wrapper = "#!/bin/sh\nexec '\(python)' -I '\(fake)' \"$@\"\n"
        func install(_ dir: String) throws {
            try Self.mkdir(dir)
            try Self.write(wrapper, to: dir + "/codex", mode: 0o755)
        }
        var explicit: String?
        let codex = s["codex"] as? String ?? ""
        let kind = codex.split(separator: ":", maxSplits: 1).first.map(String.init) ?? ""
        let arg = codex.contains(":") ? String(codex[codex.index(after: codex.firstIndex(of: ":")!)...]) : ""
        switch kind {
        case "fake": try install(binDir)
        case "npm-shim":
            // The shim drops the script path env passes.
            try Self.write("#!/bin/sh\nshift\nexec '\(python)' -I '\(fake)' \"$@\"\n", to: binDir + "/sayso-fake-node", mode: 0o755)
            try Self.write("#!/usr/bin/env sayso-fake-node\n", to: binDir + "/codex", mode: 0o755)
            explicit = binDir + "/codex"
        case "", "none": break
        case "explicit-missing": explicit = home + "/missing/codex"
        case "candidate":
            binDir = home + "/" + arg
            try install(binDir)
        case "nvm":
            for v in arg.split(separator: ",") { try install(home + "/.nvm/versions/node/\(v)/bin") }
        default: throw FixtureError("unknown codex setup \(codex)")
        }

        var configuredHome: String?
        codexHome = home + "/.codex"
        let codexHomeKind = s["codexHome"] as? String ?? ""
        switch codexHomeKind {
        case "", "default": break
        case "inside":
            configuredHome = home + "/custom-codex"
            codexHome = configuredHome!
        case "outside":
            configuredHome = root + "/outside"
            codexHome = configuredHome!
        case "symlink-outside":
            try Self.mkdir(root + "/outside", mode: 0o700)
            configuredHome = home + "/link"
            try FileManager.default.createSymbolicLink(atPath: configuredHome!, withDestinationPath: root + "/outside")
            codexHome = root + "/outside"
        case "file-not-folder":
            configuredHome = home + "/codex-file"
            try Self.write("", to: configuredHome!, mode: 0o600)
        default: throw FixtureError("unknown codexHome \(codexHomeKind)")
        }
        if codexHomeKind != "file-not-folder" {
            try Self.mkdir(codexHome, mode: 0o700)
            try Self.place(s["file"] as? [String: Any], codexHome: codexHome, home: home)
        }

        let path = (s["inheritedPath"] as? String) ?? (binDir + ":/usr/bin:/bin")
        var env = ["HOME": home, "PATH": path, "SAYSO_FAKE_SCENARIO": scenario, "SAYSO_FAKE_RECORD": record]
        for (k, v) in s["inheritedEnv"] as? [String: String] ?? [:] { env[k] = v }
        if let configuredHome { env["CODEX_HOME"] = configuredHome }
        // Never the machine's own /opt/homebrew/bin or /usr/local/bin.
        let home = self.home
        let candidates = CodexExecutable.candidates(home: home).filter { $0.hasPrefix(home + "/") }
        let root = self.root
        let clock = now, logs = logs
        cfg = CodexAuthConfiguration(
            margin: TimeInterval(Self.int(s["marginSeconds"], 600)),
            timeout: TimeInterval(Self.int(s["timeoutMs"], 5000)) / 1000,
            source: s["source"] as? String == "file" ? .file : .auto, executable: explicit, home: home,
            confineCodexHome: home, environment: env, candidates: candidates, now: { clock.value },
            guardAccess: { p in
                guard p.hasPrefix(root + "/") else { throw FixtureError("a path outside the case's temporary folder") }
            },
            log: { line in logs.value.append(line) })
        if let retry = s["retry"] as? [String: Any] {
            let target = codexHome + "/auth.json"
            let data = try Self.content(retry)
            cfg.onFileRetry = { try? Self.atomicWrite(data, to: target) }
        }
    }

    func cleanUp() {
        // What the fake started outlives a failing case; end it with the case, but only a process still in the group
        // it was started in: a pid freed and reused is not ours to kill.
        if let rec = try? read() {
            for pid in rec.group where rec.pids.contains(getpgid(pid)) { _ = kill(pid, SIGKILL) }
            for pid in rec.other where getpgid(pid) == pid { _ = kill(pid, SIGKILL) }
        }
        try? FileManager.default.removeItem(atPath: root)
    }

    // MARK: - Running

    func probe(_ want: [String: Any]) throws -> [String] {
        let cfg = self.cfg
        let cancelOn = setup["cancelOnRequest"] as? String
        let cancelledAt = Box<Date?>(nil)
        let start = Date()
        let status = try blocking(
            cancelWhen: cancelOn.map { method in { [self] in received(method) } }, cancelledAt: cancelledAt
        ) { try await CodexAuthStatus.probe(cfg) }
        // A cancelled probe is timed from the cancel.
        let elapsed = Date().timeIntervalSince(cancelledAt.value ?? start)
        var token: Token?
        if status.state == .ready {
            do { token = try blocking { try await status.credentials.token() } } catch {
                return ["ready, but token() failed: \(error)"]
            }
        }
        return try check(
            want, state: status.state.rawValue, error: status.error, token: token, mode: status.mode, info: status.info,
            elapsed: elapsed)
    }

    func timeline(_ steps: [[String: Any]]) throws -> [String] {
        let creds = try Credentials(cfg)
        let base = now.value
        let initial = Token(bearer: initialToken(), expires: nil, source: "fixture")
        var current: Token?
        var problems: [String] = []
        for (i, step) in steps.enumerated() {
            now.value = base.addingTimeInterval(TimeInterval(Self.int(step["at"], 0)))
            let outcome: Result<Token, Error>
            switch step["op"] as? String {
            case "token": outcome = Result { try blocking { try await creds.token() } }
            case "refreshAfter":
                let stale = step["stale"] as? String == "initial" ? initial : (current ?? Token(bearer: "", expires: nil))
                outcome = Result { try blocking { try await creds.refresh(after: stale) } }
            default: return ["unknown op"]
            }
            var token: Token?
            var failure: Error?
            switch outcome {
            case .success(let t):
                token = t
                current = t
            case .failure(let e): failure = e
            }
            let info = try blocking { await creds.info() }
            let found = try check(
                step["expect"] as? [String: Any] ?? [:], state: CodexAuthError.state(of: failure).rawValue,
                error: failure, token: token, mode: "", info: info, elapsed: 0)
            problems += found.map { "step \(i): \($0)" }
        }
        return problems
    }

    /// The token the scripted server first hands out: its getAuthStatus reply to includeToken without refreshToken.
    private func initialToken() -> String {
        let server = setup["server"] as? [String: Any] ?? [:]
        let entries = (server["byParams"] as? [String: Any])?["getAuthStatus"] as? [[String: Any]] ?? []
        for e in entries {
            let when = e["when"] as? [String: Any] ?? [:]
            guard (when["includeToken"] as? Bool) == true, (when["refreshToken"] as? Bool) == false,
                let first = (e["steps"] as? [[String: Any]])?.first, let result = first["result"] as? [String: Any]
            else { continue }
            return Fixtures.substitute(result["authToken"] ?? "") as? String ?? ""
        }
        return ""
    }

    // MARK: - Checking

    private struct Recorded {
        var spawns: [(argv: [Any], env: [String: String])] = []
        var requests: [[String: Any]] = []
        var pids: [Int32] = []
        /// Processes the fake started in its own process group, and in a session of their own.
        var group: [Int32] = []
        var other: [Int32] = []
    }

    private func recordLines() throws -> [[String: Any]] {
        guard let data = FileManager.default.contents(atPath: record) else { return [] }
        return try data.split(separator: 0x0A).filter { !$0.isEmpty }.map {
            guard let o = try JSONSerialization.jsonObject(with: Data($0)) as? [String: Any] else {
                throw FixtureError("a record line is not an object")
            }
            return o
        }
    }

    /// Whether the fake has recorded a request with `method`; a record caught mid-write reads as not yet.
    func received(_ method: String) -> Bool {
        (try? read())?.requests.contains { $0["method"] as? String == method } ?? false
    }

    private func read() throws -> Recorded {
        var r = Recorded()
        for line in try recordLines() {
            if let spawn = line["spawn"] as? [String: Any] {
                r.spawns.append((spawn["argv"] as? [Any] ?? [], spawn["env"] as? [String: String] ?? [:]))
                r.pids.append((line["pid"] as? NSNumber)?.int32Value ?? 0)
            } else if let req = line["req"] as? [String: Any] {
                r.requests.append(req)
            } else if let pid = (line["groupDescendant"] as? NSNumber)?.int32Value, pid > 0 {
                r.group.append(pid)
            } else if let pid = (line["descendant"] as? NSNumber)?.int32Value, pid > 0 {
                r.other.append(pid)
            }
        }
        return r
    }

    private func subst(_ s: String) -> String {
        s.replacingOccurrences(of: "$home", with: home).replacingOccurrences(of: "$codexHome", with: codexHome)
            .replacingOccurrences(of: "$binDir", with: binDir)
    }

    /// Compares one outcome with `want`: `state` is the reported state, `error` the failure (nil for none).
    func check(
        _ want: [String: Any], state: String, error: Error?, token: Token?, mode: String, info: CodexAuthInfo,
        elapsed: TimeInterval
    ) throws -> [String] {
        let rec = try read()
        var p: [String] = []
        let errorClass = ErrorClass.of(error)?.rawValue ?? ""
        let description = error.map(String.init(describing:)) ?? ""
        for (key, v) in want {
            switch key {
            case "state":
                if state != v as? String { p.append("state \(state), want \(v)") }
            case "class":
                if errorClass != v as? String { p.append("class \(errorClass.debugDescription), want \(v)") }
            case "token":
                if token?.bearer != Fixtures.substitute(v) as? String { p.append("token differs") }
            case "accountId":
                let got: Any = token?.accountID ?? NSNull()
                if !Self.same(got, v) { p.append("accountId \(got), want \(v)") }
            case "expiresAt":
                let got: Any = token?.expires.map { NSNumber(value: Int64($0.timeIntervalSince1970)) } ?? NSNull()
                if !Self.same(got, v) { p.append("expiresAt \(got), want \(v)") }
            case "mode":
                if mode != v as? String { p.append("mode \(mode.debugDescription), want \(v)") }
            case "codexVersion":
                if info.codexVersion != v as? String { p.append("codexVersion \(info.codexVersion), want \(v)") }
            case "spawns":
                if !Self.same(rec.spawns.count, v) { p.append("spawns \(rec.spawns.count), want \(v)") }
            case "argv":
                for s in rec.spawns where !Self.same(s.argv, v) { p.append("argv \(s.argv), want \(v)") }
            case "requests":
                p += Self.checkRequests(rec.requests, v as? [Any] ?? [])
            case "envAbsent":
                for name in v as? [String] ?? [] where rec.spawns.contains(where: { $0.env[name] != nil }) {
                    p.append("\(name) reached the child")
                }
            case "envEquals":
                for (name, val) in v as? [String: Any] ?? [:] {
                    let expected = subst("\(val)")
                    if rec.spawns.contains(where: { $0.env[name] != expected }) { p.append("child \(name) differs") }
                }
            case "pathStartsWith":
                let prefix = (v as? [String] ?? []).map(subst)
                for s in rec.spawns {
                    let got = (s.env["PATH"] ?? "").split(separator: ":", omittingEmptySubsequences: false).map(String.init)
                    if got.count < prefix.count || Array(got[0..<prefix.count]) != prefix {
                        p.append("child PATH does not start with the expected folders")
                    }
                }
            case "binDir":
                let got = info.bin.isEmpty ? "." : (info.bin as NSString).deletingLastPathComponent
                if got != subst("\(v)") { p.append("binDir differs") }
            case "childrenExited":
                for pid in rec.pids where pid > 0 && kill(pid, 0) == 0 { p.append("child \(pid) still alive") }
                // A member of the child's group is reparented when the child ends, so its new parent reaps it: allow
                // it a moment to disappear after the signal that ended it.
                for pid in rec.group {
                    let deadline = Date().addingTimeInterval(1)
                    while kill(pid, 0) == 0 && Date() < deadline { usleep(10_000) }
                    if kill(pid, 0) == 0 { p.append("process \(pid) of the child's group still alive") }
                }
            case "closeWithinMs":
                let limit = ((v as? NSNumber)?.doubleValue ?? 0) / 1000
                if elapsed > limit { p.append("took \(elapsed) s, limit \(limit) s") }
            case "serverRequestAnswered":  // with the method-not-found error
                let answered = rec.requests.contains {
                    $0["method"] == nil && $0["id"] != nil
                        && Self.same(($0["error"] as? [String: Any])?["code"] ?? NSNull(), -32601)
                }
                if !answered { p.append("the server request got no error reply") }
            case "outbound":  // every message the fake read, in order: a method, or "refusal:<id>" for an answer
                let got = rec.requests.map { m -> String in
                    if let method = m["method"] as? String { return method }
                    return "refusal:\(m["id"].map { "\($0)" } ?? "nil")"
                }
                if !Self.same(got, v) { p.append("outbound \(got), want \(v)") }
            case "refreshCalls":
                let n = rec.requests.filter {
                    $0["method"] as? String == "getAuthStatus"
                        && (($0["params"] as? [String: Any])?["refreshToken"] as? Bool) == true
                }.count
                if !Self.same(n, v) { p.append("refreshCalls \(n), want \(v)") }
            case "notInDescription":
                let texts = [description] + logs.value
                for s in v as? [String] ?? [] where texts.contains(where: { $0.contains(s) }) {
                    p.append("\(s.debugDescription) leaked")
                }
            case "inLog":  // strings some log line so far must contain
                for s in v as? [String] ?? [] where !logs.value.contains(where: { $0.contains(s) }) {
                    p.append("no log line names \(s.debugDescription)")
                }
            case "inDescription":
                for s in v as? [String] ?? [] where !description.contains(s) {
                    p.append("the description does not name \(s.debugDescription): \(description)")
                }
            case "stderr":  // what the child wrote to stderr, carried by a failed start; never in the description
                let got = (error as? CodexAuthError)?.stderr ?? ""
                if got != v as? String { p.append("stderr \(got.debugDescription), want \(v)") }
            case "retryAt":  // Unix seconds; null for none
                let at = (error as? CodexAuthError)?.retryAt
                let got: Any = at.map { NSNumber(value: Int64($0.timeIntervalSince1970)) } ?? NSNull()
                if !Self.same(got, v) { p.append("retryAt \(got), want \(v)") }
            default:
                p.append("unknown expectation \(key)")
            }
        }
        return p
    }

    /// Compares the requests and notifications the fake received (the client's answers to server requests excluded)
    /// with `want`.
    private static func checkRequests(_ got: [[String: Any]], _ want: [Any]) -> [String] {
        let calls = got.filter { $0["method"] != nil }
        guard calls.count == want.count else {
            return ["requests \(calls.map { $0["method"] as? String ?? "?" }), want \(want.count)"]
        }
        var p: [String] = []
        for (i, w) in want.enumerated() {
            let wm = w as? [String: Any] ?? [:]
            let m = calls[i]
            if m["method"] as? String != wm["method"] as? String {
                p.append("request \(i) is \(m["method"] ?? "?"), want \(wm["method"] ?? "?")")
            }
            let hasID = m["id"] != nil
            if (wm["notification"] as? Bool ?? false) == hasID { p.append("request \(i): notification \(!hasID)") }
            if let params = wm["params"], !same(m["params"] ?? NSNull(), params) {
                p.append("request \(i) params differ")
            }
        }
        return p
    }

    /// Whether `got` and `want` have the same JSON meaning.
    static func same(_ got: Any, _ want: Any) -> Bool {
        func text(_ v: Any) -> String? {
            (try? JSONSerialization.data(withJSONObject: [v], options: [.sortedKeys, .fragmentsAllowed]))
                .map { String(decoding: $0, as: UTF8.self) }
        }
        func norm(_ v: Any) -> Any {
            if let n = v as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID() { return n.doubleValue }
            if let a = v as? [Any] { return a.map(norm) }
            if let o = v as? [String: Any] { return o.mapValues(norm) }
            return v
        }
        guard let a = text(norm(got)), let b = text(norm(want)) else { return false }
        return a == b
    }

    // MARK: - Setup helpers

    static func lookPath(_ name: String) -> String? {
        for dir in (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":") {
            let p = "\(dir)/\(name)"
            if FileManager.default.isExecutableFile(atPath: p) { return p }
        }
        return nil
    }

    static func int(_ v: Any?, _ def: Int64) -> Int64 {
        let n = (v as? NSNumber)?.int64Value ?? 0
        return n == 0 ? def : n
    }

    static func mkdir(_ path: String, mode: Int = 0o755) throws {
        try FileManager.default.createDirectory(
            atPath: path, withIntermediateDirectories: true, attributes: [.posixPermissions: mode])
    }

    static func write(_ text: String, to path: String, mode: Int) throws {
        try Data(text.utf8).write(to: URL(fileURLWithPath: path))
        try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: path)
    }

    static func place(_ f: [String: Any]?, codexHome: String, home: String) throws {
        guard let f else { return }
        let target = codexHome + "/auth.json"
        switch f["kind"] as? String {
        case "missing": return
        case "regular": try atomicWrite(content(f), to: target)
        case "symlink":
            let elsewhere = home + "/elsewhere/auth.json"
            try mkdir(home + "/elsewhere", mode: 0o700)
            try atomicWrite(content(f), to: elsewhere)
            try FileManager.default.createSymbolicLink(atPath: target, withDestinationPath: elsewhere)
        case "fifo":
            guard mkfifo(target, 0o600) == 0 else { throw FixtureError("mkfifo failed") }
        case "directory": try mkdir(target, mode: 0o700)
        default: throw FixtureError("unknown file kind")
        }
    }

    /// The bytes a file object describes: `raw`, `hex`, `rawRepeat`, or `json` (with `$jwt` substituted, compact,
    /// padded with spaces to `padTo` bytes).
    static func content(_ f: [String: Any]) throws -> Data {
        if let raw = f["raw"] as? String { return Data(raw.utf8) }
        if let hex = f["hex"] as? String {
            let digits = Array(hex.utf8)
            guard digits.count % 2 == 0 else { throw FixtureError("odd hex length") }
            var out = Data()
            for i in stride(from: 0, to: digits.count, by: 2) {
                guard let b = UInt8(String(decoding: digits[i..<i + 2], as: UTF8.self), radix: 16) else {
                    throw FixtureError("bad hex")
                }
                out.append(b)
            }
            return out
        }
        if let rep = f["rawRepeat"] as? [String: Any] {
            return Data(String(repeating: rep["text"] as? String ?? "", count: (rep["count"] as? Int) ?? 0).utf8)
        }
        guard let json = f["json"] else { return Data() }
        var data = try JSONSerialization.data(
            withJSONObject: Fixtures.substitute(json), options: [.sortedKeys, .withoutEscapingSlashes, .fragmentsAllowed])
        let pad = ((f["padTo"] as? Int) ?? 0) - data.count
        if pad > 0 { data.append(Data(repeating: 0x20, count: pad)) }
        return data
    }

    static func atomicWrite(_ data: Data, to path: String) throws {
        let tmp = path + ".tmp"
        try data.write(to: URL(fileURLWithPath: tmp))
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: tmp)
        guard rename(tmp, path) == 0 else { throw FixtureError("rename failed") }
    }
}
#endif
