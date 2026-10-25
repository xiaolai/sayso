import CodexAuth
import Foundation
import SaySoAI
import Testing
@testable import SaySoAICLI

/// **The reader's Codex CLI as a boxed one-shot** (ADR-0021 d.3, d.8, d.9): sayso's auth states map to typed errors, the
/// exec turn carries only the flags that close the box, and a hang or a cancellation ends the whole process group. The
/// probe is the test seam — no sign-in is touched and no token is fetched here.
struct CodexCLIProviderTests {
    static func stub(hang: Bool = false) throws -> (CLIStub, URL) {
        let stub = try CLIStub()
        var body = stub.recordingPreamble()
        body += """
        if [ "$1" = "login" ]; then
          echo 'Open https://example.invalid/device and enter ABCD'
          [ -f "$D/stderr" ] && cat "$D/stderr" >&2
          exit $(cat "$D/exit" 2>/dev/null || echo 0)
        fi
        """ + "\n"
        if hang { body += "sleep 30\n" }
        body += """
        out=""; prev=""
        for a in "$@"; do
          if [ "$prev" = "-o" ]; then out="$a"; fi
          prev="$a"
        done
        if [ -n "$out" ]; then cat "$D/response" > "$out"; fi
        [ -f "$D/stderr" ] && cat "$D/stderr" >&2
        exit $(cat "$D/exit" 2>/dev/null || echo 0)
        """ + "\n"
        return (stub, try stub.script("codex", body: body))
    }

    static func provider(_ stub: CLIStub, _ url: URL, version: String = "0.163.0",
                         timeout: Duration = .seconds(30)) -> CodexCLIProvider {
        let configuration = CodexCLIProvider.Configuration(
            executable: url, codexHome: stub.directory.appending(path: "codex-home"),
            workingDirectory: stub.directory.appending(path: "work"), timeout: timeout)
        return CodexCLIProvider(configuration: configuration) { _ in
            CodexCLIProvider.Probe(state: .ready, bin: url.path, version: version)
        }
    }

    @Test func everyAuthStateMapsToItsError() {
        #expect(CodexCLIProvider.mapped(.ready) == nil)
        #expect(CodexCLIProvider.mapped(.noCodex) == .notInstalled)
        #expect(CodexCLIProvider.mapped(.codexTooOld) == .versionTooOld)
        for state in [CodexAuthState.notSignedIn, .noSignInFile, .expired] {
            #expect(CodexCLIProvider.mapped(state) == .notSignedIn, "\(state)")
        }
        for state in [CodexAuthState.apiKey, .unsupportedAuth, .appServerAuthUnavailable, .providerWithoutOpenAIAuth] {
            #expect(CodexCLIProvider.mapped(state) == .unsupportedAuth, "\(state)")
        }
        #expect(CodexCLIProvider.mapped(.codexError) == .framework("codex"))
    }

    @Test func theAskIsOneShotAndBoxed() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"outcome":"matched","quotes":["2. (noun) a ship's hold."],"explanation":"Storage."}"#)
        let text = try await Self.provider(stub, url).answer(to: "What does hold mean here?")
        #expect(text == #"{"outcome":"matched","quotes":["2. (noun) a ship's hold."],"explanation":"Storage."}"#)

        let ask = try stub.invocation(1)
        #expect(ask.arguments.first == "exec")
        for flag in ["--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "read-only"] {
            #expect(ask.arguments.contains(flag), "missing \(flag): \(ask.arguments)")
        }
        #expect(!ask.arguments.contains("app-server"), "no server is started")
        #expect(!ask.arguments.contains("resume"))
        #expect(ask.arguments.last == "-", "the prompt arrives on stdin")
        #expect(ask.environment["CODEX_HOME"] == stub.directory.appending(path: "codex-home").path)
        #expect(ask.environment["OPENAI_API_KEY"] == nil)
        #expect(ask.environment["CODEX_API_KEY"] == nil)
        #expect(ask.environment["ANTHROPIC_API_KEY"] == nil)
        #expect(CLIStub.logical(ask.workingDirectory) == stub.directory.appending(path: "work").path)
        #expect(ask.stdin == "What does hold mean here?")
    }

    @Test func anOldCodexIsRefusedBeforeItIsAsked() async throws {
        let (stub, url) = try Self.stub()
        let provider = Self.provider(stub, url, version: "0.162.0")
        do {
            _ = try await provider.answer(to: "q")
            Issue.record("an old Codex answered")
        } catch {
            #expect(error == .versionTooOld)
        }
        #expect(!FileManager.default.fileExists(atPath: stub.directory.appending(path: "argv.1").path))
    }

    @Test func aHangTimesOutAndTheChildIsGone() async throws {
        let (stub, url) = try Self.stub(hang: true)
        // Generous: the script's first execution carries the system's one-time check of a new file.
        let provider = Self.provider(stub, url, timeout: .seconds(2))
        do {
            _ = try await provider.answer(to: "q")
            Issue.record("a hang returned an answer")
        } catch {
            #expect(error == .timeout)
        }
        let pid = try stub.invocation(1).pid
        #expect(await eventuallyOffMain { isGone(pid) }, "the child outlived its timeout (pid \(pid))")
    }

    @Test func cancellationEndsTheChild() async throws {
        let (stub, url) = try Self.stub(hang: true)
        let provider = Self.provider(stub, url, timeout: .seconds(30))
        let task = Task { try await provider.answer(to: "q") }
        guard let pid = await stub.waitForStart(1) else {
            Issue.record("the stub never started")
            task.cancel()
            return
        }
        task.cancel()
        switch await task.result {
        case .success:
            Issue.record("a cancelled ask returned an answer")
        case .failure(let error):
            // `Task { }` takes an untyped-throwing operation, so its failure is `any Error`; the ask's own type is here.
            if let failure = error as? AIProviderError {
                #expect(failure == .cancelled)
            } else {
                Issue.record("unexpected error \(error)")
            }
        }
        #expect(await eventuallyOffMain { isGone(pid) }, "the child outlived its cancellation (pid \(pid))")
    }

    @Test func loginSurfacesTheDocumentedURL() async throws {
        let (stub, url) = try Self.stub()
        // **The sign-in instructions may be on either stream**, and the reader must see them while the child still
        // runs: the login passes every line of both to `onLine` (finding, 2026-10-25).
        try stub.write("stderr", "Code: WXYZ\n")
        let lines = Locked<[String]>([])
        let output = try await Self.provider(stub, url).login { line in lines.withLock { $0.append(line) } }
        let seen = lines.withLock { $0 }
        #expect(seen.contains { $0.contains("https://example.invalid/device") }, "lines: \(seen)")
        #expect(seen.contains { $0.contains("Code: WXYZ") }, "stderr was not streamed: \(seen)")
        #expect(output.contains("https://example.invalid/device"))
        let login = try stub.invocation(1)
        #expect(login.arguments == ["login"])
        #expect(login.environment["CODEX_HOME"] == stub.directory.appending(path: "codex-home").path)
    }

    /// **The app's home exists before anything reads it** (2026-10-25): on a fresh install `codexHome` does not exist
    /// yet, and a probe against a home that is not there used to fail as `.codexError` — the generic
    /// `.framework("codex")`, never the real state a reader could act on. `check` and `login` must create it first.
    @Test func theAppsHomeExistsBeforeTheProbeReadsIt() async throws {
        let (stub, url) = try Self.stub()
        let codexHome = stub.directory.appending(path: "codex-home")
        let existedAtProbe = Locked(false)
        let configuration = CodexCLIProvider.Configuration(
            executable: url, codexHome: codexHome, workingDirectory: stub.directory.appending(path: "work"))
        let provider = CodexCLIProvider(configuration: configuration) { _ in
            existedAtProbe.withLock { $0 = FileManager.default.fileExists(atPath: codexHome.path) }
            return CodexCLIProvider.Probe(state: .ready, bin: url.path, version: "0.163.0")
        }
        _ = try await provider.check()
        #expect(existedAtProbe.withLock { $0 }, "check's probe ran before the app's home was created")
    }

    @Test func theAppsHomeExistsBeforeLoginProbesIt() async throws {
        let (stub, url) = try Self.stub()
        let codexHome = stub.directory.appending(path: "codex-home")
        let existedAtProbe = Locked(false)
        let configuration = CodexCLIProvider.Configuration(
            executable: url, codexHome: codexHome, workingDirectory: stub.directory.appending(path: "work"))
        let provider = CodexCLIProvider(configuration: configuration) { _ in
            existedAtProbe.withLock { $0 = FileManager.default.fileExists(atPath: codexHome.path) }
            return CodexCLIProvider.Probe(state: .ready, bin: url.path, version: "0.163.0")
        }
        _ = try await provider.login()
        #expect(existedAtProbe.withLock { $0 }, "login's probe ran before the app's home was created")
    }

    @Test func aSecretInStderrNeverEntersADescription() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("stderr", "sk-SECRET-XYZ\n")
        try stub.write("exit", "1")
        do {
            _ = try await Self.provider(stub, url).answer(to: "q")
            Issue.record("no error")
        } catch {
            let descriptions = [String(describing: error), String(reflecting: error), "\(error)"]
            #expect(!descriptions.contains { $0.contains("sk-SECRET-XYZ") })
        }
    }
}
