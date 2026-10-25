#if os(macOS)
import CodexAuth
import Foundation
import SaySoAI

/// **A local Codex CLI as a provider**: one fresh `codex exec` process per ask, in an app-owned `CODEX_HOME`, so none
/// of the reader's config, MCP servers, hooks or instructions is inherited. The sign-in is the CLI's documented
/// `codex login` in that home; this library never writes a credential file itself.
///
/// `CodexAuth` supplies executable discovery, the version floor and the typed auth states, which are mapped to this
/// target's errors; the token it fetches is discarded unread.
public struct CodexCLIProvider: AIProvider {
    public struct Configuration: Sendable, Equatable {
        /// An explicitly chosen binary; nil lets sayso discover one.
        public let executable: URL?
        /// The app's own Codex home: its sign-in, and nothing of the reader's configuration.
        public let codexHome: URL
        /// An app-owned empty folder the CLI runs in.
        public let workingDirectory: URL
        public let model: String?
        /// The whole budget of one ask, probe included.
        public let timeout: Duration

        public init(executable: URL? = nil, codexHome: URL, workingDirectory: URL, model: String? = nil,
                    timeout: Duration = .seconds(180)) {
            self.executable = executable
            self.codexHome = codexHome
            self.workingDirectory = workingDirectory
            self.model = model
            self.timeout = timeout
        }
    }

    /// What a probe found: sayso's state, the resolved executable and its version. Internal, so sayso's type stays out
    /// of this target's public surface.
    struct Probe: Sendable, Equatable {
        let state: CodexAuthState
        let bin: String
        let version: String

        init(state: CodexAuthState, bin: String, version: String) {
            self.state = state
            self.bin = bin
            self.version = version
        }
    }

    /// The oldest Codex this provider runs: the measured 0.163.0 has every flag below (`--ephemeral`,
    /// `--ignore-user-config`, `--ignore-rules`, `--output-schema`); sayso's own floor, 0.161.0, remains the sign-in
    /// contract's.
    public static let minimumVersion = "0.163.0"
    /// The login's deadline: the reader completes a browser sign-in.
    public static let loginTimeout: Duration = .seconds(300)

    public let id = AIProviderID(kind: .codexCLI)
    private let configuration: Configuration
    /// `true` asks for a client built now rather than the long-lived one — a sign-out made a moment ago would still
    /// read as ready from a cached token, and `check` is the reader's question about now.
    private let probe: @Sendable (Bool) async -> Probe
    /// The long-lived client, replaceable after a sign-in: the old one's cached token, refusal or backoff belongs to
    /// the state before it. Nil in the test seam.
    private let clients: ClientBox?

    /// Builds sayso's client for this private home, lazily: a configuration its client refuses is reported by `check`
    /// and `answer` as `.framework("codex")`.
    public init(configuration: Configuration) {
        self.configuration = configuration
        // **The probe runs in the same restricted environment the ask does**: a sign-in read with the app's whole
        // environment would honour proxy or credential variables the ask never sees. **The CLI is the source**: this
        // provider runs it, so a sign-in file left without one is `.noCodex`, never a ready state with no version.
        let environment = CLIProviderSupport.baseEnvironment()
        let auth = CodexAuthConfiguration(
            margin: 60, timeout: 20, source: .codex, executable: configuration.executable?.path,
            codexHome: configuration.codexHome.path, home: NSHomeDirectory(),
            confineCodexHome: configuration.codexHome.path, environment: environment, candidates: nil,
            clientName: "SaySo")
        let clients = ClientBox()
        self.clients = clients
        self.probe = { [clients, auth] fresh in
            let client: Credentials?
            if fresh {
                client = try? Credentials(auth)
            } else {
                client = clients.current { try Credentials(auth) }
            }
            guard let client else { return Probe(state: .codexError, bin: "", version: "") }
            // **`info()` is read after the fetch**: the resolved binary and its version are what the fetch found, not
            // what the client knew before it looked.
            do {
                _ = try await client.token()
                let info = await client.info()
                return Probe(state: .ready, bin: info.bin, version: info.codexVersion)
            } catch let error as CodexAuthError {
                let info = await client.info()
                return Probe(state: error.state, bin: info.bin, version: error.codexVersion)
            } catch {
                let info = await client.info()
                return Probe(state: .codexError, bin: info.bin, version: info.codexVersion)
            }
        }
    }

    /// The test seam: a probe supplied directly, no sayso client and no sign-in touched.
    init(configuration: Configuration, probe: @escaping @Sendable (Bool) async -> Probe) {
        self.configuration = configuration
        self.probe = probe
        self.clients = nil
    }

    /// **Whether this source can run, without asking a model**: the sign-in state, the version floor and the app's own
    /// home. Returns the version, which the caller may show. The status is read fresh, never from a cached token.
    public func check() async throws(AIProviderError) -> String {
        let (_, found) = try await ready(fresh: true, until: ContinuousClock.now + configuration.timeout)
        return found.version
    }

    public func answer(to question: String) async throws(AIProviderError) -> String {
        guard !Task.isCancelled else { throw .cancelled }
        let clock = ContinuousClock()
        let deadline = clock.now + configuration.timeout
        let executable = try await ready(fresh: false, until: deadline).0
        // **The probe spends the same budget the ask does**: what is left after it is what the run gets.
        let remaining = try CLIProviderSupport.remaining(until: deadline, clock: clock)
        let message = configuration.workingDirectory.appending(path: "last-message-\(UUID().uuidString).txt")
        defer { try? FileManager.default.removeItem(at: message) }
        let environment = Self.environment(configuration, executable: executable)
        let run: CLIProcess.Run
        do {
            run = try await CLIProcess.run(
                executable: executable, arguments: Self.arguments(configuration, message: message),
                environment: environment, workingDirectory: configuration.workingDirectory, input: question,
                deadline: remaining)
        } catch {
            guard !Task.isCancelled else { throw .cancelled }
            throw CLIProviderSupport.mapped(error)
        }
        guard !Task.isCancelled else { throw .cancelled }
        guard run.status == 0 else { throw Self.classify(run) }
        return try Self.text(at: message)
    }

    /// **The documented login, in the app's own home**: `codex login`, its output (the URL and code, on either stream)
    /// passed to `onLine` as it arrives, and returned whole. This library writes no credential file and reads none.
    public func login(onLine: @escaping @Sendable (String) -> Void = { _ in }) async throws(AIProviderError) -> String {
        guard !Task.isCancelled else { throw .cancelled }
        // **The app's home exists before anything reads it**: `codex login` writes its sign-in there.
        try CLIProviderSupport.prepare([configuration.codexHome, configuration.workingDirectory])
        let found = await probe(true)
        guard !Task.isCancelled else { throw .cancelled }
        let executable = try Self.resolve(configuration, found)
        let environment = Self.environment(configuration, executable: executable)
        let run: CLIProcess.Run
        do {
            run = try await CLIProcess.run(
                executable: executable, arguments: ["login"], environment: environment,
                workingDirectory: configuration.workingDirectory, input: nil, deadline: Self.loginTimeout,
                onLine: onLine)
        } catch {
            guard !Task.isCancelled else { throw .cancelled }
            throw CLIProviderSupport.mapped(error)
        }
        // The client that watched the old state must not answer for the new one, even when the caller has moved on.
        if run.status == 0 { clients?.replace() }
        guard !Task.isCancelled else { throw .cancelled }
        guard run.status == 0 else { throw Self.classify(run) }
        return run.stdout + run.stderr
    }

    // MARK: - Preparation

    /// **The one readiness path**, for both `check` and `answer`: the app's own folders, the sign-in state, the
    /// version floor and the resolution. `fresh` decides whether the status comes from a client built now or the
    /// long-lived one; every other step is the same, so the two questions cannot drift apart.
    ///
    /// **The folders exist before the probe reads them** (2026-10-25): on a fresh install, `codexHome` does not
    /// exist yet, and a probe against a home that is not there fails as `.codexError` — the generic
    /// `.framework("codex")`, never the real `.notSignedIn` a reader can act on.
    private func ready(fresh: Bool, until deadline: ContinuousClock.Instant) async throws(AIProviderError) -> (URL, Probe) {
        guard !Task.isCancelled else { throw .cancelled }
        try CLIProviderSupport.prepare([configuration.codexHome, configuration.workingDirectory])
        let found = try await Self.bounded(until: deadline) { [probe] in await probe(fresh) }
        guard !Task.isCancelled else { throw .cancelled }
        if let error = Self.mapped(found.state) { throw error }
        guard CLIVersion.atLeast(found.version, Self.minimumVersion) else { throw .versionTooOld }
        let executable = try Self.resolve(configuration, found)
        return (executable, found)
    }

    /// **The probe within the ask's deadline**: past it, the probe is cancelled (sayso's client honours cancellation)
    /// and the ask is `.timeout`; a cancelled ask is `.cancelled`.
    private static func bounded(until deadline: ContinuousClock.Instant,
                                _ probe: @escaping @Sendable () async -> Probe) async throws(AIProviderError) -> Probe {
        let found = await withTaskGroup(of: Probe?.self) { group in
            group.addTask { await probe() }
            group.addTask {
                try? await Task.sleep(until: deadline, clock: .continuous)
                return nil
            }
            let first = await group.next() ?? nil
            group.cancelAll()
            return first
        }
        guard let found else { throw Task.isCancelled ? .cancelled : .timeout }
        return found
    }

    /// The one-shot, boxed turn: no session on disk, no user config or rules, a read-only sandbox, no approvals, and the
    /// answer written to a file of our own. The prompt arrives on stdin; no shell ever sees it.
    static func arguments(_ configuration: Configuration, message: URL) -> [String] {
        var arguments = [
            "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules",
            // **No tools at all**: `-s read-only` stops writes but still lets the model run shell commands, so text
            // injected into the prompt could ask it to read a local file. Both built-in tool sets are disabled; the
            // prompt is data on stdin and the answer is the only thing that leaves.
            "--disable", "shell_tool", "--disable", "browser_use",
            "-s", "read-only", "-C", configuration.workingDirectory.path,
            "-c", #"approval_policy="never""#, "-o", message.path,
        ]
        if let model = configuration.model, !model.isEmpty { arguments += ["-m", model] }
        arguments.append("-")
        return arguments
    }

    // MARK: - Mapping

    /// sayso's state as this target's error; nil for `ready`.
    static func mapped(_ state: CodexAuthState) -> AIProviderError? {
        switch state {
        case .ready: nil
        case .noCodex: .notInstalled
        case .codexTooOld: .versionTooOld
        case .notSignedIn, .noSignInFile, .expired: .notSignedIn
        case .apiKey, .unsupportedAuth, .appServerAuthUnavailable, .providerWithoutOpenAIAuth: .unsupportedAuth
        case .codexError: .framework("codex")
        }
    }

    static func classify(_ run: CLIProcess.Run) -> AIProviderError {
        CLIProviderSupport.classify(run.stdout + "\n" + run.stderr, provider: "codex", status: run.status,
                                    signIn: ["codex login"])
    }

    // MARK: - Files and environment

    /// **One resolver for every choice**: an explicit path or name, the path a probe found, and discovery all go through
    /// `CodexExecutable.resolve` — a regular file, executable by this process, and not changeable by others. An explicit
    /// choice that does not resolve is a refusal, never a fall-through to another binary.
    static func resolve(_ configuration: Configuration, _ found: Probe) throws(AIProviderError) -> URL {
        let path = ProcessInfo.processInfo.environment["PATH"] ?? ""
        if let explicit = configuration.executable {
            guard let resolved = CodexExecutable.resolve(explicit: explicit.path, path: path, candidates: []) else {
                throw .notInstalled
            }
            return URL(fileURLWithPath: resolved)
        }
        // **The probed binary or none**: its version is the one checked, and a replacement found by discovery was never
        // probed.
        if !found.bin.isEmpty {
            guard let resolved = CodexExecutable.resolve(explicit: found.bin, path: path, candidates: []) else {
                throw .notInstalled
            }
            return URL(fileURLWithPath: resolved)
        }
        let resolved = CodexExecutable.resolve(
            explicit: nil, path: path, candidates: CodexExecutable.candidates(home: NSHomeDirectory()))
        guard let resolved else { throw .notInstalled }
        return URL(fileURLWithPath: resolved)
    }

    /// sayso's own rule (`CodexExecutable.childEnvironment`): the app's environment minus every authentication
    /// override, `CODEX_HOME` replaced by the app's own, `PATH` repaired so an npm-installed CLI finds `node`.
    static func environment(_ configuration: Configuration, executable: URL) -> [String: String] {
        CodexExecutable.childEnvironment(
            CLIProviderSupport.baseEnvironment(), bin: executable.path, codexHome: configuration.codexHome.path)
    }

    /// The final message `codex exec` wrote, bounded **while reading**: a huge or hostile file must not be loaded
    /// before its size is known.
    static func text(at url: URL) throws(AIProviderError) -> String {
        guard let handle = try? FileHandle(forReadingFrom: url) else { throw .badShape }
        defer { try? handle.close() }
        guard let data = try? handle.read(upToCount: CLIProcess.stdoutCap + 1), data.count <= CLIProcess.stdoutCap,
              let text = String(data: data, encoding: .utf8),
              !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else { throw .badShape }
        return text
    }
}

/// **The long-lived Codex client, replaceable.** After a sign-in, the old client's cached token, refusal or backoff all
/// belong to the state before it; a successful login starts a new one. Under a lock because the provider is a value and
/// several asks may share it.
private final class ClientBox: @unchecked Sendable {
    private let lock = NSLock()
    private var client: Credentials?

    func current(_ make: () throws -> Credentials) -> Credentials? {
        lock.lock()
        defer { lock.unlock() }
        if let client { return client }
        guard let made = try? make() else { return nil }
        client = made
        return made
    }

    func replace() {
        lock.lock()
        client = nil
        lock.unlock()
    }
}
#endif
