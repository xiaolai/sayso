#if os(macOS)
import Foundation
import RealtimeConnection

/// How to obtain the token (spec/credentials.md).
public struct CodexAuthConfiguration: Sendable {
    /// Where the token comes from.
    public enum Source: String, Sendable, Equatable {
        /// Codex when it resolves, else auth.json.
        case auto
        /// Codex only.
        case codex
        /// auth.json only.
        case file
    }

    public var source: Source
    /// An explicitly chosen executable (a path, or a name looked up in `PATH`); nil resolves.
    public var executable: String?
    /// nil means `CODEX_HOME` from `environment`, else unset.
    public var codexHome: String?
    /// The user's home folder; nil means the process's.
    public var home: String?
    /// When set, the folder a `CODEX_HOME` must resolve inside for auth.json to be read (a sandboxed app confines it
    /// to the home folder). Ownership is checked either way.
    public var confineCodexHome: String?
    /// The inherited environment; nil means the process's.
    public var environment: [String: String]?
    /// The folders searched after `PATH`; nil means ``CodexExecutable/candidates(home:)``.
    public var candidates: [String]?
    /// How much lifetime a token must have left; below it, it is refreshed before use. Required.
    public var margin: TimeInterval
    /// Bounds every app-server call. Required.
    public var timeout: TimeInterval
    public var now: @Sendable () -> Date
    /// Sent to Codex as `clientInfo.name`.
    public var clientName: String
    /// Runs before an executable is started or auth.json is read, with its path; a throw refuses. Keeps unit tests
    /// away from the real Codex and the real sign-in.
    public var guardAccess: (@Sendable (String) throws -> Void)?
    /// Runs before the one re-read of auth.json after a parse failure. For tests.
    public var onFileRetry: (@Sendable () -> Void)?
    /// Receives debug lines, which never hold a token or the child's output.
    public var log: (@Sendable (String) -> Void)?

    public init(
        margin: TimeInterval, timeout: TimeInterval, source: Source = .auto, executable: String? = nil,
        codexHome: String? = nil, home: String? = nil, confineCodexHome: String? = nil,
        environment: [String: String]? = nil, candidates: [String]? = nil, now: @escaping @Sendable () -> Date = { Date() },
        clientName: String = "sayso", guardAccess: (@Sendable (String) throws -> Void)? = nil,
        onFileRetry: (@Sendable () -> Void)? = nil, log: (@Sendable (String) -> Void)? = nil
    ) {
        self.margin = margin
        self.timeout = timeout
        self.source = source
        self.executable = executable
        self.codexHome = codexHome
        self.home = home
        self.confineCodexHome = confineCodexHome
        self.environment = environment
        self.candidates = candidates
        self.now = now
        self.clientName = clientName
        self.guardAccess = guardAccess
        self.onFileRetry = onFileRetry
        self.log = log
    }
}

/// An invalid ``CodexAuthConfiguration``.
public struct CodexAuthConfigurationError: Error, Sendable, Equatable, CustomStringConvertible {
    public let description: String
    public init(_ description: String) { self.description = description }
}

/// What the latest fetch found.
public struct CodexAuthInfo: Sendable, Equatable {
    /// The resolved executable; "" when none.
    public var bin = ""
    /// The explicit executable that did not resolve.
    public var missingBin = ""
    /// Where the latest token came from, or was looked for: codex or file; nil before a fetch.
    public var source: CodexAuthConfiguration.Source?
    /// The `CODEX_HOME` in force; "" when unset.
    public var codexHome = ""
    /// "" when unknown.
    public var codexVersion = ""

    public init(
        bin: String = "", missingBin: String = "", source: CodexAuthConfiguration.Source? = nil, codexHome: String = "",
        codexVersion: String = ""
    ) {
        self.bin = bin
        self.missingBin = missingBin
        self.source = source
        self.codexHome = codexHome
        self.codexVersion = codexVersion
    }
}

/// Hands out the ChatGPT bearer Codex holds for this machine and owns the refresh policy: single-flight, a refresh
/// succeeds when the token it returns is fresh, and a failure backs off from 60 s, doubling to 15 min, without a
/// permanent budget. Codex runs only during a fetch or a refresh, on a private serial queue, never on the caller's
/// thread. Thrown errors are ``CodexAuthError``.
public final class Credentials: CredentialSource, @unchecked Sendable, CustomStringConvertible {
    static let backoffStart: TimeInterval = 60
    static let backoffMax: TimeInterval = 15 * 60

    private let cfg: CodexAuthConfiguration
    private let env: [String: String]
    private let home: String
    private let codexHome: String?
    private let candidates: [String]
    private let path: String
    private let queue = DispatchQueue(label: "sayso.codexauth.credentials")

    // State, touched only on `queue`.
    private var current: Token?
    private var refreshable = false
    private var backoff: TimeInterval = 0
    private var retryAt: Date?
    private var latest = CodexAuthInfo()
    /// Asks for a refreshed token; ``refreshCodex()``, replaced only by tests.
    var refresher: (() throws -> Token)?

    /// Validates `cfg`; the first use fetches.
    public init(_ cfg: CodexAuthConfiguration) throws {
        guard cfg.margin > 0, cfg.timeout > 0 else {
            throw CodexAuthConfigurationError("codexauth: margin and timeout are required")
        }
        self.cfg = cfg
        env = cfg.environment ?? ProcessInfo.processInfo.environment
        home = cfg.home ?? NSHomeDirectory()
        let ch = cfg.codexHome ?? env["CODEX_HOME"]
        codexHome = (ch?.isEmpty ?? true) ? nil : ch
        path = env["PATH"] ?? ""
        candidates = cfg.candidates ?? CodexExecutable.candidates(home: home)
    }

    public var description: String { "Credentials{\(cfg.source.rawValue)}" }

    /// What the latest fetch found.
    public func info() async -> CodexAuthInfo {
        await withCheckedContinuation { cont in queue.async { cont.resume(returning: self.latest) } }
    }

    /// A fresh bearer, fetching on first use and refreshing when less than the margin remains. A token from auth.json
    /// is re-read rather than refreshed.
    public func token() async throws -> Token {
        try await run { try self.tokenLocked() }
    }

    /// Called when a request made with `stale` was refused as auth-expired. When `stale` was already replaced, the
    /// replacement is returned without a refresh; otherwise a refresh runs, within the backoff.
    public func refresh(after stale: Token) async throws -> Token {
        try await run { try self.refreshAfterLocked(stale) }
    }

    private func run<T: Sendable>(_ body: @escaping @Sendable () throws -> T) async throws -> T {
        try await withCheckedThrowingContinuation { cont in
            queue.async { cont.resume(with: Result { try body() }) }
        }
    }

    // MARK: - On the queue

    private func fresh(_ t: Token) -> Bool {
        guard let expires = t.expires else { return true }  // unknown expiry: the provider decides
        return expires.timeIntervalSince(cfg.now()) >= cfg.margin
    }

    private func tokenLocked() throws -> Token {
        if current == nil || (!refreshable && !fresh(current!)) { try fetchLocked() }
        if fresh(current!) { return current! }
        return try refreshLocked()
    }

    private func refreshAfterLocked(_ stale: Token) throws -> Token {
        if let current, current.bearer != stale.bearer { return current }
        if current == nil || !refreshable {
            try fetchLocked()
            if current!.bearer != stale.bearer && fresh(current!) { return current! }
            if !refreshable {
                throw CodexAuthError(
                    .expired, message: "auth.json holds the refused token, and the file source cannot refresh")
            }
        }
        return try refreshLocked()
    }

    private func refreshLocked() throws -> Token {
        guard refreshable else {
            throw CodexAuthError(
                .expired, message: "the token in auth.json is expiring, and the file source cannot refresh")
        }
        let now = cfg.now()
        if let retryAt, now < retryAt {
            throw CodexAuthError(.expired, message: "the last refresh failed; the next attempt waits for the backoff")
        }
        do {
            let tok = try (refresher ?? refreshCodex)()
            guard fresh(tok) else {
                throw CodexAuthConfigurationError("Codex returned a token that is still within the margin")
            }
            backoff = 0
            retryAt = nil
            current = tok
            return tok
        } catch {
            backoff = backoff == 0 ? Self.backoffStart : min(2 * backoff, Self.backoffMax)
            retryAt = now.addingTimeInterval(backoff)
            throw CodexAuthError(
                .expired, message: "the refresh did not renew the sign-in", cause: String(describing: error))
        }
    }

    private func fetchLocked() throws {
        do {
            let (tok, canRefresh) = try fetch()
            current = tok
            refreshable = canRefresh
        } catch var e as CodexAuthError {
            e.codexVersion = latest.codexVersion
            throw e
        }
    }

    private func fetch() throws -> (Token, Bool) {
        latest = CodexAuthInfo(codexHome: codexHome ?? "")
        if cfg.source == .file { return (try fileToken(fallback: false), false) }
        guard let bin = CodexExecutable.resolve(explicit: cfg.executable, path: path, candidates: candidates) else {
            if !(cfg.executable ?? "").isEmpty || cfg.source == .codex {
                latest.missingBin = cfg.executable ?? ""
                throw CodexAuthError(.noCodex, message: "no Codex executable resolved")
            }
            return (try fileToken(fallback: true), false)
        }
        latest.bin = bin
        return try codexToken(bin)
    }

    /// Reads auth.json. `fallback` marks the file source as the fallback for a missing executable: a missing or
    /// unusable file is then no-codex, because with no usable file either the fix is to install Codex.
    private func fileToken(fallback: Bool) throws -> Token {
        latest.source = .file
        func fallbackState(_ e: CodexAuthError) -> CodexAuthError {
            guard fallback, e.state == .noSignInFile || e.state == .codexError else { return e }
            return CodexAuthError(
                .noCodex, message: "no Codex executable resolved and auth.json is missing or unusable",
                cause: String(describing: e))
        }
        let file: String
        do { file = try AuthFile.path(codexHome: codexHome, home: home, confine: cfg.confineCodexHome) } catch let e
            as CodexAuthError
        { throw fallbackState(e) }
        if let guardAccess = cfg.guardAccess {
            do { try guardAccess(file) } catch {
                throw CodexAuthError(
                    .codexError, message: "reading auth.json was refused", cause: String(describing: error))
            }
        }
        do { return try AuthFile.read(file, onRetry: cfg.onFileRetry) } catch let e as CodexAuthError {
            throw fallbackState(e)
        }
    }

    private func start(_ bin: String) throws -> AppServerClient {
        let client: AppServerClient
        do {
            client = try AppServerClient.start(
                .init(
                    bin: bin, codexHome: codexHome, environment: env, timeout: cfg.timeout, clientName: cfg.clientName,
                    guardStart: cfg.guardAccess, log: cfg.log))
        } catch {
            throw CodexAuthError(
                .codexError, message: "Codex app-server did not start", cause: String(describing: error))
        }
        let version = Self.version(client.userAgent)
        latest.codexVersion = version
        if !version.isEmpty, CodexExecutable.versionLess(version, minimumCodexVersion) {
            client.close()
            throw CodexAuthError(.codexTooOld, codexVersion: version, message: "Codex is older than \(minimumCodexVersion)")
        }
        return client
    }

    /// Asks a fresh app-server child: metadata first, and the token only for a ChatGPT sign-in, so an API key is
    /// never requested.
    private func codexToken(_ bin: String) throws -> (Token, Bool) {
        latest.source = .codex
        let client = try start(bin)
        defer { client.close() }
        let meta: AuthStatus
        do { meta = try client.getAuthStatus(includeToken: false, refreshToken: false) } catch let e as AppServerError
            where e.rpcCode == AppServerError.methodNotFound
        {
            client.close()
            return (try storeFallback(bin), false)
        } catch {
            throw CodexAuthError(
                .codexError, message: "Codex app-server getAuthStatus failed", cause: String(describing: error))
        }
        guard meta.requiresOpenAIAuth else {
            throw CodexAuthError(.providerWithoutOpenAIAuth, message: "the provider in force needs no OpenAI sign-in")
        }
        if let e = classifyMode(meta.method) { throw e }
        return (try Self.token(from: { try client.getAuthStatus(includeToken: true, refreshToken: false) }), true)
    }

    /// Runs when Codex lacks getAuthStatus: a fresh child's config/read decides whether auth.json is Codex's
    /// authority. A running app-server keeps the store it started with, so the answer comes from a new child.
    private func storeFallback(_ bin: String) throws -> Token {
        let client = try start(bin)
        let store: String
        do {
            store = try client.credentialsStore()
            client.close()
        } catch let e as AppServerError where e.rpcCode == AppServerError.methodNotFound {
            client.close()
            // Without config/read the store cannot be confirmed to be the file, so the file is not Codex's authority.
            throw CodexAuthError(
                .appServerAuthUnavailable, message: "Codex lacks getAuthStatus and config/read",
                cause: String(describing: e))
        } catch {
            client.close()
            throw CodexAuthError(
                .codexError, message: "Codex app-server config/read failed", cause: String(describing: error))
        }
        if !store.isEmpty && store != "file" {
            throw CodexAuthError(
                .appServerAuthUnavailable, message: "Codex lacks getAuthStatus and keeps its sign-in in \(store)")
        }
        return try fileToken(fallback: false)
    }

    /// Asks a fresh child to refresh the sign-in.
    private func refreshCodex() throws -> Token {
        guard let bin = CodexExecutable.resolve(explicit: cfg.executable, path: path, candidates: candidates) else {
            throw CodexAuthError(.noCodex, message: "no Codex executable resolved")
        }
        let client = try start(bin)
        defer { client.close() }
        return try Self.token(from: { try client.getAuthStatus(includeToken: true, refreshToken: true) })
    }

    /// Checks the method again on a token reply: a changed method is classified like the metadata reply.
    private static func token(from call: () throws -> AuthStatus) throws -> Token {
        let full: AuthStatus
        do { full = try call() } catch {
            throw CodexAuthError(
                .codexError, message: "Codex app-server getAuthStatus failed", cause: String(describing: error))
        }
        if let e = classifyMode(full.method) { throw e }
        guard !full.token.isEmpty else {
            throw CodexAuthError(.codexError, message: "Codex reported a ChatGPT sign-in without a token")
        }
        return makeToken(full.token, source: "codex")
    }

    /// The Codex version in an initialize userAgent such as "codex_cli_rs/0.162.1 (Mac OS 15.0; arm64)"; "" when
    /// there is none.
    public static func version(_ userAgent: String) -> String {
        guard let slash = userAgent.firstIndex(of: "/") else { return "" }
        let rest = userAgent[userAgent.index(after: slash)...].prefix { $0 == "." || ("0"..."9").contains($0) }
        guard let first = rest.first, first != "." else { return "" }
        return String(rest)
    }
}

/// The outcome of a probe. It never holds the token in a printable field.
public struct CodexAuthStatus: Sendable {
    public let state: CodexAuthState
    public let info: CodexAuthInfo
    /// The sign-in mode: "chatgpt" when ready, else as ``CodexAuthError/mode``.
    public let mode: String
    /// The token's expiry when ready; nil when unknown.
    public let expires: Date?
    /// The failure of a non-ready state, for diagnostics.
    public let error: CodexAuthError?
    /// The credentials the probe used. It hands out the token when the state is ready, and a later call may succeed
    /// where this one failed (after a sign-in, or after the refresh backoff).
    public let credentials: Credentials

    public init(
        state: CodexAuthState, info: CodexAuthInfo, mode: String, expires: Date?, error: CodexAuthError?,
        credentials: Credentials
    ) {
        self.state = state
        self.info = info
        self.mode = mode
        self.expires = expires
        self.error = error
        self.credentials = credentials
    }

    /// Obtains the token once and classifies the outcome. Throws only for an invalid configuration.
    public static func probe(_ cfg: CodexAuthConfiguration) async throws -> CodexAuthStatus {
        let c = try Credentials(cfg)
        do {
            let tok = try await c.token()
            return CodexAuthStatus(
                state: .ready, info: await c.info(), mode: modeChatGPT, expires: tok.expires, error: nil,
                credentials: c)
        } catch {
            let e = (error as? CodexAuthError) ?? CodexAuthError(.codexError, cause: String(describing: error))
            return CodexAuthStatus(
                state: e.state, info: await c.info(), mode: e.mode, expires: nil, error: e, credentials: c)
        }
    }
}
#endif
