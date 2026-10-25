import Foundation
import SaySoAI
import Testing
@testable import SaySoAICLI

/// **A Claude CLI as a boxed one-shot**: the argument vector, the environment, the working directory and stdin are all
/// asserted from the stub's own record; a hang, a cancellation, a sign-in problem and a leaky stderr each end typed.
struct ClaudeCLIProviderTests {
    static func stub(version: String = "2.1.304", hang: Bool = false) throws -> (CLIStub, URL) {
        let stub = try CLIStub()
        var body = stub.recordingPreamble()
        body += """
        if [ "$1" = "--version" ]; then echo '\(version)'; exit 0; fi
        """ + "\n"
        if hang { body += "sleep 30\n" }
        body += """
        cat "$D/response" 2>/dev/null
        [ -f "$D/stderr" ] && cat "$D/stderr" >&2
        exit $(cat "$D/exit" 2>/dev/null || echo 0)
        """ + "\n"
        return (stub, try stub.script("claude", body: body))
    }

    /// The caller's answer shape, as the provider receives it; the test reads it back out of the argument vector.
    static let schema = #"{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}"#

    static func provider(_ stub: CLIStub, _ url: URL, timeout: Duration = .seconds(30)) -> ClaudeCLIProvider {
        ClaudeCLIProvider(configuration: .init(
            executable: url, configDirectory: stub.directory.appending(path: "config"),
            workingDirectory: stub.directory.appending(path: "work"), timeout: timeout, answerSchema: Self.schema))
    }

    @Test func theAskIsABoxedOneShot() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"type":"result","subtype":"success","is_error":false,"result":"x","structured_output":{"outcome":"matched","quotes":["2. (noun) a ship's hold."],"explanation":"Here it is storage."}}"#)
        let text = try await Self.provider(stub, url).answer(to: "What does hold mean here?")
        #expect(text == #"{"explanation":"Here it is storage.","outcome":"matched","quotes":["2. (noun) a ship's hold."]}"#)

        let ask = try stub.invocation(2)   // 1 is the version probe
        for flag in ["-p", "--output-format", "json", "--json-schema", "--no-session-persistence",
                     "--strict-mcp-config", "--disable-slash-commands", "--permission-prompts", "none", "--safe-mode"] {
            #expect(ask.arguments.contains(flag), "missing \(flag): \(ask.arguments)")
        }
        #expect(!ask.arguments.contains("--resume"), "a one-shot ask must not resume")
        #expect(value(of: "--tools", in: ask.arguments) == "", "tools must be off")
        #expect(value(of: "--setting-sources", in: ask.arguments) == "", "settings must be off")
        #expect(value(of: "--permission-prompts", in: ask.arguments) == "none")
        #expect(ask.environment["CLAUDE_CONFIG_DIR"] == stub.directory.appending(path: "config").path)
        #expect(ask.environment["ANTHROPIC_API_KEY"] == nil)
        #expect(ask.environment["CLAUDE_CODE_OAUTH_TOKEN"] == nil)
        #expect(CLIStub.logical(ask.workingDirectory) == stub.directory.appending(path: "work").path)
        #expect(ask.stdin == "What does hold mean here?")
    }

    @Test func twoAsksShareNoSession() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"type":"result","subtype":"success","result":"{\"outcome\":\"refused\",\"quotes\":[],\"explanation\":\"x\"}"}"#)
        let provider = Self.provider(stub, url)
        _ = try await provider.answer(to: "first")
        _ = try await provider.answer(to: "second")
        let first = try stub.invocation(2)
        let second = try stub.invocation(4)
        for ask in [first, second] {
            #expect(ask.arguments.contains("--no-session-persistence"))
            #expect(!ask.arguments.contains("--resume"))
            #expect(!ask.arguments.contains("--session-id"))
        }
        #expect(first.stdin == "first")
        #expect(second.stdin == "second")
    }

    @Test func aSignInProblemIsTyped() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("stderr", "Not signed in. Please run /login.\n")
        try stub.write("exit", "1")
        await expect(.notSignedIn) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
    }

    @Test func anOldCLIIsRefusedBeforeItIsAsked() async throws {
        let (stub, url) = try Self.stub(version: "2.0.9")
        await expect(.versionTooOld) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
        #expect(!FileManager.default.fileExists(atPath: stub.directory.appending(path: "argv.2").path))
    }

    @Test func aMissingCLIIsNotInstalled() async throws {
        let stub = try CLIStub()
        let missing = stub.directory.appending(path: "no-such-claude")
        await expect(.notInstalled) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, missing).answer(to: "q") }
    }

    @Test func anAnswerThatIsNotJSONIsNoAnswer() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", "Sure! Here is what it means.")
        await expect(.badShape) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
    }

    /// **The envelope's verdict comes before its output** (finding, 2026-10-25): an error envelope that also carries a
    /// structured output is the CLI's failure, never a usable answer.
    @Test func anErrorEnvelopeIsNeverAnAnswer() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 429 rate limit exceeded","structured_output":{"outcome":"matched","quotes":[],"explanation":""}}"#)
        await expect(.rateLimited) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
    }

    /// An error envelope with no `result` at all is still an error — not a shape the library failed to read, and not a
    /// refusal it cannot name.
    @Test func anErrorEnvelopeWithoutAResultIsStillTyped() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"type":"result","subtype":"error","is_error":true}"#)
        await expect(.framework("claude error")) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
    }

    /// **A failing exit prefers the envelope's structured verdict to whatever stderr says**: the CLI's own error record
    /// is classified from its sentence, and stderr noise cannot relabel it.
    @Test func aFailingExitPrefersTheEnvelopesVerdict() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 429 rate limit exceeded"}"#)
        try stub.write("stderr", "Not signed in. Please run /login.\n")
        try stub.write("exit", "1")
        await expect(.rateLimited) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
    }

    /// **A success envelope that stopped short is not an answer**: a model refusal is `.refused` and a cut at the token
    /// ceiling is `.truncated`, whatever text `result` carries.
    @Test func aStopReasonOtherThanTheEndIsTyped() async throws {
        for (reason, wanted) in [("refusal", AIProviderError.refused), ("max_tokens", .truncated)] {
            let (stub, url) = try Self.stub()
            try stub.write("response", #"{"type":"result","subtype":"success","is_error":false,"stop_reason":"\#(reason)","result":"I can't help with that."}"#)
            await expect(wanted) { () async throws(AIProviderError) -> String in
                try await Self.provider(stub, url).answer(to: "q") }
        }
    }

    /// **The caller's schema may describe any JSON value**: an array or a scalar structured output is the answer, never
    /// discarded for `result`.
    @Test func aStructuredOutputOfAnyJSONTypeIsTheAnswer() async throws {
        for (structured, wanted) in [(#"["a","b"]"#, #"["a","b"]"#), ("42", "42"), (#""text""#, #""text""#)] {
            let (stub, url) = try Self.stub()
            try stub.write("response", #"{"type":"result","subtype":"success","is_error":false,"result":"prose","structured_output":\#(structured)}"#)
            #expect(try await Self.provider(stub, url).answer(to: "q") == wanted)
        }
    }

    /// **An error envelope's words may be in `errors`**, not `result`: a rate limit there is a rate limit, and an envelope
    /// that names nothing leaves the streams to classify.
    @Test func anErrorEnvelopesErrorsArrayIsClassified() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("response", #"{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: 429 rate limit exceeded"]}"#)
        await expect(.rateLimited) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }

        let (other, otherURL) = try Self.stub()
        try other.write("response", #"{"type":"result","subtype":"error","is_error":true,"errors":["something broke"]}"#)
        try other.write("stderr", "Not signed in. Please run /login.\n")
        try other.write("exit", "1")
        await expect(.notSignedIn) { () async throws(AIProviderError) -> String in
            try await Self.provider(other, otherURL).answer(to: "q") }
    }

    /// The floor is the version measured to have the flags this provider passes, not an earlier guess at one.
    @Test func aVersionBelowTheMeasuredFloorIsRefused() async throws {
        let (stub, url) = try Self.stub(version: "2.1.200")
        await expect(.versionTooOld) { () async throws(AIProviderError) -> String in
            try await Self.provider(stub, url).answer(to: "q") }
        #expect(!FileManager.default.fileExists(atPath: stub.directory.appending(path: "argv.2").path))
    }

    @Test func aHangTimesOutAndTheChildIsGone() async throws {
        let (stub, url) = try Self.stub(hang: true)
        // **The budget covers the probe and the ask**, so it has to be generous enough that the ask starts: the script's
        // first execution carries the system's one-time check of a new file (measured here: 50–270 ms against ~5 ms
        // afterwards), and on a loaded Mac running many test bundles at once that check alone outran 2 s — the ask never
        // launched, `argv.2` was never written, and this test failed with "argv.2 … no such file" (audit-fix
        // 2026-10-22). The ask is what must time out, not the probe.
        let provider = Self.provider(stub, url, timeout: .seconds(6))
        await expect(.timeout) { () async throws(AIProviderError) -> String in
            try await provider.answer(to: "q") }
        let pid = try stub.invocation(2).pid
        #expect(await eventuallyOffMain { isGone(pid) }, "the child outlived its timeout (pid \(pid))")
    }

    @Test func cancellationEndsTheChild() async throws {
        let (stub, url) = try Self.stub(hang: true)
        let provider = Self.provider(stub, url, timeout: .seconds(30))
        let task = Task { try await provider.answer(to: "q") }
        guard let pid = await stub.waitForStart(2) else {
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

    /// **A cancellation during cleanup is still a cancellation**: the child has exited with a sign-in failure, but the
    /// ask was cancelled while its streams were still being drained, and the caller hears `.cancelled`.
    @Test func aCancellationAfterTheChildExitsIsCancelled() async throws {
        let stub = try CLIStub()
        var body = stub.recordingPreamble()
        body += """
        if [ "$1" = "--version" ]; then echo '2.1.304'; exit 0; fi
        set -m
        sleep 0.8 &
        echo 'Not signed in. Please run /login.' >&2
        touch "$D/exited"
        exit 1
        """ + "\n"
        let url = try stub.script("claude", body: body)
        let task = Task { try await Self.provider(stub, url).answer(to: "q") }
        let exited = stub.directory.appending(path: "exited")
        let deadline = ContinuousClock.now + .seconds(10)
        while !FileManager.default.fileExists(atPath: exited.path), ContinuousClock.now < deadline {
            try await Task.sleep(for: .milliseconds(10))
        }
        try await Task.sleep(for: .milliseconds(200))
        task.cancel()
        switch await task.result {
        case .success: Issue.record("a cancelled ask returned an answer")
        case .failure(let error): #expect(error as? AIProviderError == .cancelled)
        }
    }

    @Test func aSecretInStderrNeverEntersADescription() async throws {
        let (stub, url) = try Self.stub()
        try stub.write("stderr", "sk-SECRET-XYZ something failed\n")
        try stub.write("exit", "1")
        do {
            _ = try await Self.provider(stub, url).answer(to: "q")
            Issue.record("no error")
        } catch {
            let descriptions = [String(describing: error), String(reflecting: error), "\(error)"]
            #expect(!descriptions.contains { $0.contains("sk-SECRET-XYZ") })
        }
    }

    // MARK: - Helpers

    private func value(of flag: String, in arguments: [String]) -> String? {
        guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count else { return nil }
        return arguments[index + 1]
    }

    private func expect(_ wanted: AIProviderError, _ body: @escaping () async throws(AIProviderError) -> String) async {
        do {
            _ = try await body()
            Issue.record("expected \(wanted.code), got an answer")
        } catch {
            #expect(error == wanted, "expected \(wanted.code), got \(error.code)")
        }
    }
}
