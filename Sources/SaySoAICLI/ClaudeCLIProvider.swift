#if os(macOS)
import Foundation
import SaySoAI

/// **A local Claude CLI as a provider**: one fresh `claude -p` process per ask, in an app-owned `CLAUDE_CONFIG_DIR`,
/// with the tool, hook and MCP switches off. Claude Code writes and owns its credential for that folder in the reader's
/// Keychain (the service name it derives from the config dir); this library never reads it, never stores it, and never
/// looks at `~/.claude`.
///
/// One-shot by construction (`--no-session-persistence`, no `--resume`): the next ask cannot see this one's sentence.
public struct ClaudeCLIProvider: AIProvider {
    public struct Configuration: Sendable, Equatable {
        /// An explicitly chosen binary; nil discovers one (`PATH`, then the folders a Finder-launched app misses).
        public let executable: URL?
        /// The app's own configuration folder for Claude Code; the credential's keychain slot follows from it.
        public let configDirectory: URL
        /// An app-owned empty folder the CLI runs in.
        public let workingDirectory: URL
        public let model: String?
        /// The whole budget of one ask, probe included.
        public let timeout: Duration
        /// The search path for discovery, for tests; nil uses the process's own `PATH`.
        public let searchPath: String?
        /// **The caller's answer shape**, handed to `--json-schema`: this library does not know what an answer means, so
        /// the consumer supplies its own schema and receives the CLI's structured output as text.
        public let answerSchema: String

        public init(executable: URL? = nil, configDirectory: URL, workingDirectory: URL, model: String? = nil,
                    timeout: Duration = .seconds(120), searchPath: String? = nil, answerSchema: String) {
            self.executable = executable
            self.configDirectory = configDirectory
            self.workingDirectory = workingDirectory
            self.model = model
            self.timeout = timeout
            self.searchPath = searchPath
            self.answerSchema = answerSchema
        }
    }

    /// The oldest CLI this provider runs: `--json-schema` and `--safe-mode` are both in it. Measured on 2026-10-20:
    /// `claude` 2.1.304 — the floor is that measured version, not an earlier guess at what might have worked.
    public static let minimumVersion = "2.1.304"

    public let id = AIProviderID(kind: .claudeCLI)
    private let configuration: Configuration

    public init(configuration: Configuration) {
        self.configuration = configuration
    }

    /// **Whether this source can run, without asking a model**: the executable found, its version at or over the floor,
    /// and the app's own folders prepared. Returns the version, which the caller may show.
    public func check() async throws(AIProviderError) -> String {
        let clock = ContinuousClock()
        let (_, version) = try await Self.prepared(configuration, clock: clock,
                                                   deadline: clock.now + configuration.timeout)
        return version
    }

    public func answer(to question: String) async throws(AIProviderError) -> String {
        guard !Task.isCancelled else { throw .cancelled }
        let clock = ContinuousClock()
        let deadline = clock.now + configuration.timeout
        let (executable, _) = try await Self.prepared(configuration, clock: clock, deadline: deadline)
        // **The probe spends the same budget the ask does**: what is left after the version check is what the ask gets.
        guard !Task.isCancelled else { throw .cancelled }
        let remaining = try CLIProviderSupport.remaining(until: deadline, clock: clock)
        // The lean, measured set, plus what makes this ask a closed box: no tools, no MCP, no settings, no slash
        // commands, nobody to answer a permission prompt, no session on disk. `--bare` is deliberately absent: it reads
        // no OAuth, and the reader's sign-in is the point.
        var arguments = [
            "-p", "--output-format", "json", "--json-schema", configuration.answerSchema,
            "--no-session-persistence", "--tools", "", "--strict-mcp-config", "--setting-sources", "",
            "--disable-slash-commands", "--permission-prompts", "none", "--safe-mode",
        ]
        if let model = configuration.model, !model.isEmpty { arguments += ["--model", model] }
        let environment = Self.environment(configuration, executable: executable)
        let run: CLIProcess.Run
        do {
            run = try await CLIProcess.run(
                executable: executable, arguments: arguments, environment: environment,
                workingDirectory: configuration.workingDirectory, input: question, deadline: remaining)
        } catch {
            guard !Task.isCancelled else { throw .cancelled }
            throw CLIProviderSupport.mapped(error)
        }
        // **A cancelled ask is cancelled even if the child finished**, before its status is read: neither its answer
        // nor its failure is handed to a reader who has moved on.
        guard !Task.isCancelled else { throw .cancelled }
        guard run.status == 0 else { throw Self.classify(run) }
        return try Self.text(from: run.stdout)
    }

    // MARK: - Preparation

    /// The executable, its version and the app's folders — what both `check` and `answer` need, on one deadline.
    static func prepared(_ configuration: Configuration, clock: ContinuousClock,
                         deadline: ContinuousClock.Instant) async throws(AIProviderError) -> (URL, String) {
        let executable = try Self.discover(configuration)
        let left = try CLIProviderSupport.remaining(until: deadline, clock: clock)
        let version = try await Self.version(of: executable,
                                             environment: Self.environment(configuration, executable: executable),
                                             deadline: min(left, .seconds(15)))
        guard Self.atLeast(version, Self.minimumVersion) else { throw .versionTooOld }
        try CLIProviderSupport.prepare([configuration.configDirectory, configuration.workingDirectory])
        return (executable, version)
    }

    static func discover(_ configuration: Configuration) throws(AIProviderError) -> URL {
        if let executable = configuration.executable {
            guard CLIProcess.isExecutable(executable) else { throw .notInstalled }
            return executable
        }
        let repair = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
        let inherited = configuration.searchPath ?? ProcessInfo.processInfo.environment["PATH"] ?? ""
        let home = NSHomeDirectory()
        let gui = [home + "/.local/bin", home + "/.npm-global/bin", home + "/.volta/bin", home + "/.bun/bin",
                   home + "/Library/pnpm"]
        let folders = (inherited + ":" + repair).split(separator: ":").map(String.init) + gui
        for folder in folders where !folder.isEmpty {
            let candidate = URL(fileURLWithPath: folder).appending(path: "claude")
            if CLIProcess.isExecutable(candidate) { return candidate }
        }
        throw .notInstalled
    }

    static func version(of executable: URL, environment: [String: String],
                        deadline: Duration) async throws(AIProviderError) -> String {
        let run: CLIProcess.Run
        do {
            run = try await CLIProcess.run(
                executable: executable, arguments: ["--version"], environment: environment,
                workingDirectory: URL(fileURLWithPath: NSTemporaryDirectory()), deadline: deadline)
        } catch {
            throw CLIProviderSupport.mapped(error)
        }
        // **Both print forms**: `2.1.304` and a prefixed `codex-cli 0.163.0` — the first token that starts with a digit.
        guard run.status == 0,
              let token = run.stdout.split(whereSeparator: \.isWhitespace).first(where: { $0.first?.isNumber == true })
        else {
            throw .framework("claude --version")
        }
        return String(token)
    }

    /// Numeric components, compared as integers (sayso's rule); a non-numeric component is 0.
    static func atLeast(_ version: String, _ minimum: String) -> Bool {
        CLIVersion.atLeast(version, minimum)
    }

    // MARK: - The box

    /// The child's whole environment: the app's home and temp, a repaired `PATH` (an npm-installed CLI needs `node`),
    /// and the app's config folder. Nothing else — no `ANTHROPIC_*`, no key the app happens to hold.
    static func environment(_ configuration: Configuration, executable: URL) -> [String: String] {
        var environment = CLIProviderSupport.baseEnvironment()
        let folder = executable.deletingLastPathComponent().path
        environment["PATH"] = folder + ":" + (environment["PATH"] ?? "/usr/bin:/bin")
        environment["CLAUDE_CONFIG_DIR"] = configuration.configDirectory.path
        return environment
    }

    // MARK: - The answer

    /// The model's answer text: `structured_output` when the CLI returns it, else `result`. **The envelope's own verdict
    /// comes first**: an error envelope is a refusal whatever it carries — a structured output included.
    /// A turn that stopped at a refusal or the token ceiling is no answer, whatever `result` says. The structured output
    /// is any JSON value the caller's schema describes, not only an object.
    static func text(from stdout: String) throws(AIProviderError) -> String {
        guard let object = Self.envelope(stdout) else { throw .badShape }
        if let refusal = Self.envelopeError(object) { throw refusal }
        switch object["stop_reason"] as? String {
        case "refusal": throw .refused
        case "max_tokens": throw .truncated
        default: break
        }
        if let structured = object["structured_output"], !(structured is NSNull) {
            guard let encoded = try? JSONSerialization.data(withJSONObject: structured,
                                                            options: [.sortedKeys, .fragmentsAllowed]) else {
                throw .badShape
            }
            return String(decoding: encoded, as: UTF8.self)
        }
        guard let result = object["result"] as? String, !result.isEmpty else { throw .badShape }
        return result
    }

    /// The CLI's JSON envelope, when it wrote one.
    static func envelope(_ stdout: String) -> [String: Any]? {
        guard let data = stdout.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return nil }
        return object
    }

    /// **The envelope's own verdict**: an error envelope is a refusal whatever it carries, classified from its own
    /// sentence rather than from whatever else the streams happen to hold. The sentence is the envelope's `errors`,
    /// with `result` as a fallback.
    static func envelopeError(_ object: [String: Any]) -> AIProviderError? {
        let isError = object["is_error"] as? Bool ?? false
        let subtype = object["subtype"] as? String
        guard isError || (subtype != nil && subtype != "success") else { return nil }
        let errors = (object["errors"] as? [Any])?.compactMap { $0 as? String } ?? []
        let words = (errors + [object["result"] as? String ?? ""]).joined(separator: "\n")
        return Self.classify(text: words, status: 0)
    }

    /// A failing run's own words: **the structured verdict first**; the raw streams when the CLI wrote no envelope, or
    /// one whose words name nothing this build knows.
    static func classify(_ run: CLIProcess.Run) -> AIProviderError {
        if let object = Self.envelope(run.stdout), let refusal = Self.envelopeError(object),
           refusal != .framework("claude error") {
            return refusal
        }
        return Self.classify(text: run.stdout + "\n" + run.stderr, status: run.status)
    }

    static func classify(text: String, status: Int32) -> AIProviderError {
        CLIProviderSupport.classify(text, provider: "claude", status: status)
    }
}
#endif
