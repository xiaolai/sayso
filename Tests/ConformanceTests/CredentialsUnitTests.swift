#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import RealtimeConnection
import XCTest

/// What the credentials fixtures cannot observe: reflection, process lifetime, write deadlines, the refresh policy
/// under concurrency, cancellation, and the folder a read goes through.
final class CredentialsUnitTests: XCTestCase {
    private func dumped(_ v: Any) -> String {
        var s = ""
        dump(v, to: &s)
        return s + String(reflecting: v) + "\(v)"
    }

    private func jwt(_ exp: Int) -> String {
        func b(_ s: String) -> String {
            Data(s.utf8).base64EncodedString().replacingOccurrences(of: "+", with: "-")
                .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
        }
        return b(#"{"alg":"none"}"#) + "." + b(#"{"exp":\#(exp)}"#) + ".c2ln"
    }

    func testReflectionNeverShowsTokensOrTheEnvironment() throws {
        let status = AuthStatus(method: "chatgpt", requiresOpenAIAuth: true, token: "opaque-canary-reflect-1")
        XCTAssertFalse(dumped(status).contains("canary"), "AuthStatus reflection shows the token")
        let env = ["OPENAI_API_KEY": "canary-env-reflect-2", "PATH": "/usr/bin"]
        let appCfg = AppServerClient.Configuration(bin: "/b/codex", environment: env, timeout: 1)
        XCTAssertFalse(dumped(appCfg).contains("canary"), "AppServerClient.Configuration reflection shows the environment")
        let cfg = CodexAuthConfiguration(margin: 60, timeout: 1, source: .file, environment: env)
        XCTAssertFalse(dumped(cfg).contains("canary"), "CodexAuthConfiguration reflection shows the environment")
        let creds = try Credentials(cfg)
        XCTAssertFalse(dumped(creds).contains("canary"), "Credentials reflection shows the environment")
    }

    func testErrorDescriptionIsScrubbedWhole() {
        Secret.register("canary-mode-3")
        var e = CodexAuthError(.codexError, message: "ek_canarymessage")
        e.mode = "canary-mode-3"
        XCTAssertFalse(e.description.contains("canary"))
        XCTAssertFalse(cleanMode("e\u{0}k_canary4").contains("canary"), "a secret joined by filtering survived")
        XCTAssertEqual(cleanMode(String(repeating: "é", count: 70) + "\u{7}"), String(repeating: "é", count: 64))
    }

    func testConfigurationRejectsInfiniteBounds() {
        XCTAssertThrowsError(try Credentials(CodexAuthConfiguration(margin: .infinity, timeout: 1)))
        XCTAssertThrowsError(try Credentials(CodexAuthConfiguration(margin: 60, timeout: .infinity)))
    }

    func testEmptyExplicitHomeTakesTheInheritedOne() async throws {
        let cfg = CodexAuthConfiguration(
            margin: 60, timeout: 1, source: .file, codexHome: "", home: NSTemporaryDirectory(),
            environment: ["CODEX_HOME": "/nonexistent-sayso-home"], guardAccess: { _ in throw CancellationError() })
        let status = try await CodexAuthStatus.probe(cfg)
        XCTAssertEqual(status.info.codexHome, "/nonexistent-sayso-home")
    }

    func testExecutableResolution() throws {
        let root = NSTemporaryDirectory() + "sayso-exec-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: root) }
        for d in ["a", "b"] {
            try FileManager.default.createDirectory(atPath: "\(root)/\(d)", withIntermediateDirectories: true)
            FileManager.default.createFile(atPath: "\(root)/\(d)/codex", contents: Data("#!/bin/sh\n".utf8))
            chmod("\(root)/\(d)/codex", 0o755)
        }
        if getuid() != 0 {
            chmod("\(root)/a/codex", 0o641)  // executable by others only
            XCTAssertEqual(CodexExecutable.resolve(explicit: nil, path: "\(root)/a:\(root)/b", candidates: []), "\(root)/b/codex")
        }
        XCTAssertNil(CodexExecutable.resolve(explicit: "\(root)/b/codex\u{0}suffix", path: "", candidates: []))
        XCTAssertFalse(CodexExecutable.versionLess("0.99999999999999999999.0", "0.162.0"), "an overflowing part is not 0")
        XCTAssertTrue(CodexExecutable.versionLess("0.161.9", "0.162.0"))

        // An nvm version that is a link to a folder is not a version folder, as in Go.
        let home = "\(root)/home"
        let node = "\(home)/.nvm/versions/node"
        try FileManager.default.createDirectory(atPath: "\(node)/v20.0.0/bin", withIntermediateDirectories: true)
        try FileManager.default.createDirectory(atPath: "\(root)/real/bin", withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(atPath: "\(node)/v22.0.0", withDestinationPath: "\(root)/real")
        let nvm = CodexExecutable.candidates(home: home).filter { $0.hasPrefix(node) }
        XCTAssertEqual(nvm, ["\(node)/v20.0.0/bin"])
    }

    /// A Codex is refused when every user can change it, as in Go: the file is writable by all, or a folder read on
    /// the way to it, a link's own folder included, is; a sticky folder of this user's is not such a folder. Owners
    /// and group write are not judged. A search skips such a Codex and takes the next; an explicit one is refused.
    func testCodexEveryoneCanChangeIsRefused() throws {
        let tmp = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
        let root = tmp + "/sayso-writable-\(UUID().uuidString)"
        let dirs: [(String, mode_t)] = [
            ("safe", 0o755), ("safe/bin", 0o755), ("safe/lib", 0o755), ("open", 0o777), ("open/deep", 0o755),
            ("sticky", 0o1777), ("group", 0o775),
        ]
        defer {
            for (d, _) in dirs { chmod(root + "/" + d, 0o755) }
            try? FileManager.default.removeItem(atPath: root)
        }
        for (d, _) in dirs { try FileManager.default.createDirectory(atPath: root + "/" + d, withIntermediateDirectories: true) }
        for (f, mode) in [
            ("safe/codex", 0o755), ("safe/lib/codex.js", 0o755), ("safe/all", 0o757), ("open/codex", 0o755),
            ("open/deep/codex", 0o755), ("sticky/codex", 0o755), ("group/codex", 0o775),
        ] as [(String, mode_t)] {
            XCTAssert(FileManager.default.createFile(atPath: root + "/" + f, contents: Data("#!/bin/sh\n".utf8)))
            XCTAssertEqual(chmod(root + "/" + f, mode), 0)
        }
        for (l, target) in [
            ("safe/bin/codex", "../lib/codex.js"), ("safe/to-open", "../open/codex"), ("open/to-safe", "../safe/codex"),
            ("safe/loop", "loop"), ("safe/abs", root + "/safe/lib/codex.js"),
        ] {
            try FileManager.default.createSymbolicLink(atPath: root + "/" + l, withDestinationPath: target)
        }
        for (d, mode) in dirs { XCTAssertEqual(chmod(root + "/" + d, mode), 0) }
        for (path, want) in [
            ("safe/codex", false), ("safe/bin/codex", false), ("safe/abs", false), ("sticky/codex", false),
            ("group/codex", false), ("safe/all", true), ("open/codex", true), ("open/deep/codex", true),
            ("open/to-safe", true), ("safe/to-open", true), ("safe/loop", true), ("safe/missing", true), ("safe", true),
        ] {
            XCTAssertEqual(CodexExecutable.changeableByOthers(root + "/" + path), want, path)
        }
        XCTAssertEqual(
            CodexExecutable.resolve(explicit: nil, path: root + "/open:" + root + "/safe", candidates: []),
            root + "/safe/codex")
        XCTAssertNil(CodexExecutable.resolve(explicit: root + "/open/codex", path: "", candidates: []))
    }

    /// Callers that refused a token while a refresh ran take its result, even when Codex returned the same bearer.
    func testRefusalsShareARefreshThatReturnedTheSameBearer() async throws {
        let now = Date(timeIntervalSince1970: 1_800_000_000)
        let creds = try Credentials(
            CodexAuthConfiguration(margin: 60, timeout: 1, source: .codex, environment: [:], now: { now }))
        let same = Token(bearer: jwt(1_800_003_600), expires: Date(timeIntervalSince1970: 1_800_003_600))
        creds.seedForTesting(same, refreshable: true)
        let calls = Counter()
        creds.refresher = {
            calls.increment()
            Thread.sleep(forTimeInterval: 0.1)
            return same
        }
        try await withThrowingTaskGroup(of: Token.self) { group in
            for _ in 0..<8 { group.addTask { try await creds.refresh(after: same) } }
            for try await _ in group {}
        }
        XCTAssertEqual(calls.value, 1)
    }

    /// The backoff runs from the failure, and a refused token is not handed out again.
    func testBackoffRunsFromTheFailureAndTheRefusedTokenStaysRefused() async throws {
        let clock = Clock(1_800_000_000)
        let creds = try Credentials(
            CodexAuthConfiguration(margin: 600, timeout: 1, source: .codex, environment: [:], now: { clock.now }))
        let tok = Token(bearer: jwt(1_800_003_600), expires: Date(timeIntervalSince1970: 1_800_003_600))
        creds.seedForTesting(tok, refreshable: true)
        let calls = Counter()
        creds.refresher = {
            calls.increment()
            clock.advance(90)  // a slow failure
            throw CodexAuthError(.codexError, message: "offline")
        }
        await XCTAssertThrowsErrorAsync(try await creds.refresh(after: tok))
        clock.advance(1)
        await XCTAssertThrowsErrorAsync(try await creds.token(), "the refused token was handed out again")
        XCTAssertEqual(calls.value, 1, "a call 1 s after a slow failure refreshed again")
    }

    /// A task cancelled while it waits its turn is released without running.
    func testCancelledWhileWaiting() async throws {
        let creds = try Credentials(CodexAuthConfiguration(margin: 60, timeout: 1, source: .codex, environment: [:]))
        let tok = Token(bearer: "opaque-wait", expires: nil)
        creds.seedForTesting(tok, refreshable: true)
        let started = Counter()
        creds.refresher = {
            started.increment()
            Thread.sleep(forTimeInterval: 1)
            return Token(bearer: "opaque-wait-2", expires: nil)
        }
        let first = Task { try await creds.refresh(after: tok) }  // holds the queue for 1 s
        try await Task.sleep(nanoseconds: 100_000_000)
        let second = Task { try await creds.token() }
        try await Task.sleep(nanoseconds: 50_000_000)
        let t0 = Date()
        second.cancel()
        do {
            _ = try await second.value
            XCTFail("a cancelled waiter succeeded")
        } catch is CancellationError {
        } catch { XCTFail("waiter: \(error)") }
        XCTAssertLessThan(Date().timeIntervalSince(t0), 0.5, "the cancelled waiter waited for the queue")
        _ = try await first.value
        XCTAssertEqual(started.value, 1)
    }

    /// A refresh the caller cancels while it runs spends no backoff, and its failure keeps the state but is
    /// cancelled, never a credential failure.
    func testCancelledRefreshSpendsNoBackoff() async throws {
        let creds = try Credentials(CodexAuthConfiguration(margin: 60, timeout: 1, source: .codex, environment: [:]))
        let tok = Token(bearer: "opaque-cancel", expires: nil)
        creds.seedForTesting(tok, refreshable: true)
        let calls = Counter()
        let release = DispatchSemaphore(value: 0)
        creds.refresher = {
            calls.increment()
            _ = release.wait(timeout: .now() + 5)
            throw CodexAuthError(.codexError, message: "interrupted")
        }
        let task = Task { try await creds.refresh(after: tok) }
        for _ in 0..<500 where calls.value == 0 { try await Task.sleep(nanoseconds: 10_000_000) }
        XCTAssertEqual(calls.value, 1, "the refresh did not start")
        task.cancel()
        release.signal()
        do {
            _ = try await task.value
            XCTFail("a cancelled refresh succeeded")
        } catch let e as CodexAuthError {
            XCTAssertEqual(e.state, .expired)
            XCTAssertEqual(e.errorClass, .cancelled, "\(e)")
        }
        release.signal()  // the next refresh fails at once
        do {
            _ = try await creds.refresh(after: tok)
            XCTFail("the second refresh succeeded")
        } catch let e as CodexAuthError {
            XCTAssertEqual(e.errorClass, .authExpired, "a refresh that Codex failed is not cancelled: \(e)")
        }
        XCTAssertEqual(calls.value, 2, "the cancelled refresh set a backoff")
    }

    /// ErrorClass.of reads an error's class as Go's Classify does, through NSError wrapping too.
    func testErrorClassOf() {
        XCTAssertNil(ErrorClass.of(nil))
        XCTAssertEqual(ErrorClass.of(CancellationError()), .cancelled)
        XCTAssertEqual(ErrorClass.of(URLError(.cancelled)), .cancelled)
        XCTAssertEqual(ErrorClass.of(URLError(.timedOut)), .network)
        XCTAssertEqual(ErrorClass.of(RealtimeDeadline.Expired()), .network)
        XCTAssertEqual(ErrorClass.of(RealtimeError(.rateLimited)), .rateLimited)
        XCTAssertEqual(ErrorClass.of(CodexAuthError(.expired)), .authExpired)
        XCTAssertEqual(ErrorClass.of(CodexAuthError(.notSignedIn)), .authMode)
        XCTAssertEqual(ErrorClass.of(AppServerError("Codex app-server exited unexpectedly")), .codexError)
        XCTAssertEqual(ErrorClass.of(CocoaError(.fileReadNoSuchFile)), .internal)
        let wrapped = NSError(domain: "app", code: 1, userInfo: [NSUnderlyingErrorKey: CodexAuthError(.apiKey)])
        XCTAssertEqual(ErrorClass.of(wrapped), .authMode)
        let twice = NSError(domain: "app", code: 2, userInfo: [NSUnderlyingErrorKey: wrapped])
        XCTAssertEqual(ErrorClass.of(twice), .authMode)
    }

    /// The configurations a client refuses are programming errors, as in Go; a call the caller gave up is cancelled.
    func testAppServerRefusalsAreInternal() {
        let relative = AppServerClient.Configuration(bin: "codex", environment: [:], timeout: 1)
        XCTAssertThrowsError(try AppServerClient.start(relative)) { XCTAssertEqual(ErrorClass.of($0), .internal) }
        let noTimeout = AppServerClient.Configuration(bin: "/bin/false", environment: [:], timeout: 0)
        XCTAssertThrowsError(try AppServerClient.start(noTimeout)) { XCTAssertEqual(ErrorClass.of($0), .internal) }
    }

    /// A child that stops reading cannot hold a call past its timeout, nor close past the grace; a client released
    /// without close still ends its child.
    func testWritesHaveADeadlineAndAReleasedClientEndsItsChild() throws {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let dir = NSTemporaryDirectory() + "sayso-stall-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let script = dir + "/codex"
        let body = "#!\(python)\nimport sys, json, time\nm = json.loads(sys.stdin.readline())\n"
            + "print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\ntime.sleep(30)\n"
        FileManager.default.createFile(atPath: script, contents: Data(body.utf8))
        chmod(script, 0o755)
        // Generous enough for a loaded machine to start Python and answer initialize.
        let cfg = AppServerClient.Configuration(bin: script, environment: ["PATH": "/usr/bin:/bin"], timeout: 3)

        let c = try AppServerClient.start(cfg)
        var t0 = Date()
        XCTAssertThrowsError(try c.call("big", params: ["pad": String(repeating: "x", count: 4 << 20)]))
        XCTAssertLessThan(Date().timeIntervalSince(t0), 5, "the call outlived its timeout")
        t0 = Date()
        c.close()
        XCTAssertLessThan(Date().timeIntervalSince(t0), 4, "close outlived the grace")

        var pid: pid_t = 0
        do {
            let dropped = try AppServerClient.start(cfg)
            pid = dropped.pid
        }
        let deadline = Date().addingTimeInterval(5)
        while kill(pid, 0) == 0 && Date() < deadline { usleep(20_000) }
        XCTAssertNotEqual(kill(pid, 0), 0, "a released client left its child running")
    }

    /// A present account or planType of the wrong type is an error, as in Go.
    func testPlanTypeValidatesItsFields() throws {
        let python = try XCTUnwrap(CredentialWorld.lookPath("python3"))
        let fake = Fixtures.root.deletingLastPathComponent().appendingPathComponent("tools/fake-app-server.py").path
        let dir = NSTemporaryDirectory() + "sayso-plan-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let script = dir + "/codex"
        FileManager.default.createFile(
            atPath: script, contents: Data("#!/bin/sh\nexec '\(python)' -I '\(fake)' \"$@\"\n".utf8))
        chmod(script, 0o755)
        for (account, ok) in [("[]", false), (#"{"planType": 123}"#, false), ("null", true), (#"{"planType": "plus"}"#, true)] {
            let scenario = dir + "/s.json"
            try Data(#"{"methods": {"account/read": [{"result": {"account": \#(account)}}]}}"#.utf8)
                .write(to: URL(fileURLWithPath: scenario))
            let c = try AppServerClient.start(
                .init(bin: script, environment: ["PATH": "/usr/bin:/bin", "SAYSO_FAKE_SCENARIO": scenario], timeout: 5))
            defer { c.close() }
            if ok {
                XCTAssertNoThrow(try c.planType(), account)
            } else {
                XCTAssertThrowsError(try c.planType(), account)
            }
        }
    }

    /// A CODEX_HOME swapped for a link after it was validated cannot redirect the read.
    func testFileReadsThroughThePinnedFolder() throws {
        let base = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
        let home = base + "/sayso-pin-\(UUID().uuidString)"
        let inside = home + "/codex", outside = base + "/sayso-pin-out-\(UUID().uuidString)"
        defer {
            try? FileManager.default.removeItem(atPath: home)
            try? FileManager.default.removeItem(atPath: outside)
        }
        for d in [inside, outside] { try FileManager.default.createDirectory(atPath: d, withIntermediateDirectories: true) }
        FileManager.default.createFile(
            atPath: outside + "/auth.json", contents: Data(#"{"tokens":{"access_token":"outside-token"}}"#.utf8),
            attributes: [.posixPermissions: 0o600])
        let loc = try AuthFile.location(codexHome: inside, home: home, confine: home)
        defer { loc.close() }
        try FileManager.default.moveItem(atPath: inside, toPath: inside + ".old")
        try FileManager.default.createSymbolicLink(atPath: inside, withDestinationPath: outside)
        let tok = try? AuthFile.read(loc, onRetry: nil)
        XCTAssertNotEqual(tok?.bearer, "outside-token", "the read followed the swapped folder")
    }

    /// A folder of the resolved path replaced by a link after resolution, an ancestor as well as CODEX_HOME itself,
    /// fails the open: the path is walked from "/" without following a link, so it cannot leave the confining folder.
    func testResolvedPathIsOpenedWithoutFollowingALink() throws {
        let base = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
        let home = base + "/sayso-walk-\(UUID().uuidString)"
        let outside = base + "/sayso-walk-out-\(UUID().uuidString)"
        defer {
            try? FileManager.default.removeItem(atPath: home)
            try? FileManager.default.removeItem(atPath: outside)
        }
        let codexHome = home + "/a/b/codex"
        for d in [codexHome, outside + "/b/codex"] {
            try FileManager.default.createDirectory(atPath: d, withIntermediateDirectories: true)
        }
        let fd = try XCTUnwrap(AuthFile.openResolved(codexHome), "a resolved path holding no link opens")
        Darwin.close(fd)
        // The ancestor "a" swapped for a link to a look-alike tree outside the confining folder.
        try FileManager.default.moveItem(atPath: home + "/a", toPath: home + "/a.old")
        try FileManager.default.createSymbolicLink(atPath: home + "/a", withDestinationPath: outside)
        XCTAssertNil(AuthFile.openResolved(codexHome), "the walk followed a swapped ancestor")
        // CODEX_HOME itself swapped for a link.
        try FileManager.default.removeItem(atPath: home + "/a")
        try FileManager.default.moveItem(atPath: home + "/a.old", toPath: home + "/a")
        try FileManager.default.moveItem(atPath: codexHome, toPath: codexHome + ".old")
        try FileManager.default.createSymbolicLink(atPath: codexHome, withDestinationPath: outside + "/b/codex")
        XCTAssertNil(AuthFile.openResolved(codexHome), "the walk followed a swapped CODEX_HOME")
        XCTAssertNil(AuthFile.openResolved("a/b"), "a relative path is refused")
        XCTAssertNil(AuthFile.openResolved(home + "/a/../a/b"), "a path with .. is refused")
    }

    /// Confinement compares folders, not paths, as in Go: CODEX_HOME reached through the data volume's own mount
    /// point, which realpath keeps, or spelt in another case is inside; the folder beside it is not.
    func testConfinementComparesFoldersNotPaths() throws {
        let tmp = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
        let base = tmp + "/sayso-confine-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: base) }
        for side in ["confine", "outside"] {
            try FileManager.default.createDirectory(atPath: base + "/\(side)/a/b", withIntermediateDirectories: true)
            FileManager.default.createFile(
                atPath: base + "/\(side)/a/b/auth.json", contents: Data(#"{"tokens":{"access_token":"\#(side)-token"}}"#.utf8))
        }
        var homes = [base + "/confine/a/b"]
        let data = "/System/Volumes/Data" + base
        if FileManager.default.fileExists(atPath: data) { homes.append(data + "/confine/a/b") }
        if FileManager.default.fileExists(atPath: base + "/CONFINE") { homes.append(base + "/CONFINE/A/b") }
        for codexHome in homes {
            let loc = try AuthFile.location(codexHome: codexHome, home: base, confine: base + "/confine")
            defer { loc.close() }
            XCTAssertEqual(try AuthFile.read(loc, onRetry: nil).bearer, "confine-token", codexHome)
        }
        XCTAssertGreaterThan(homes.count, 1, "neither another mount point nor another case reaches the folder here")
        XCTAssertThrowsError(try AuthFile.location(codexHome: base + "/outside/a/b", home: base, confine: base + "/confine"))
    }

    /// A FIFO named as CODEX_HOME is refused at once instead of blocking the open.
    func testFIFOAsCodexHomeIsRefusedWithoutBlocking() throws {
        let base = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
        let home = base + "/sayso-fifo-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: home) }
        try FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
        XCTAssertEqual(mkfifo(home + "/codex", 0o600), 0)
        for confine in [home, nil] {
            XCTAssertThrowsError(try AuthFile.location(codexHome: home + "/codex", home: home, confine: confine)) {
                XCTAssertEqual(($0 as? CodexAuthError)?.state, .codexError)
            }
        }
    }
}

private final class Clock: @unchecked Sendable {
    private let lock = NSLock()
    private var t: TimeInterval
    init(_ t: TimeInterval) { self.t = t }
    var now: Date { lock.withLock { Date(timeIntervalSince1970: t) } }
    func advance(_ s: TimeInterval) { lock.withLock { t += s } }
}

private func XCTAssertThrowsErrorAsync<T>(
    _ expression: @autoclosure () async throws -> T, _ message: String = "", file: StaticString = #filePath,
    line: UInt = #line
) async {
    do {
        _ = try await expression()
        XCTFail("no error: \(message)", file: file, line: line)
    } catch {}
}
#endif
