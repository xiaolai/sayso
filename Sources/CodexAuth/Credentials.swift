#if os(macOS)
import Foundation
import RealtimeConnection

/// How to obtain the token (spec/credentials.md). Formatting and reflection never show the environment, which may hold
/// credentials.
public struct CodexAuthConfiguration: Sendable, CustomStringConvertible, CustomReflectable {
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
    /// nil or "" means `CODEX_HOME` from `environment`, else unset.
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

    public var description: String { "CodexAuthConfiguration{\(source.rawValue)}" }
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "source": source.rawValue, "executable": executable as Any, "codexHome": codexHome as Any,
                "margin": margin, "timeout": timeout, "environment": Secret.redacted,
            ], displayStyle: .struct)
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
/// thread. Thrown errors are ``CodexAuthError``, or `CancellationError` for a task cancelled while it waited its
/// turn; a task cancelled while Codex runs ends that call. Formatting and reflection show the source only.
public final class Credentials: CredentialSource, @unchecked Sendable, CustomStringConvertible, CustomReflectable {
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
    /// The bearer a provider last refused and no refresh has renewed since: it is not handed out again until a
    /// refresh succeeds or the file holds another token.
    private var refused: String?
    private var backoff: TimeInterval = 0
    private var retryAt: Date?
    /// The refresh failure that started the backoff, reported as the cause of each call it refuses, and the stderr
    /// it carries.
    private var lastFailure = ""
    private var lastStderr = ""
    private var latest = CodexAuthInfo()
    /// Counts successful refreshes; read off the queue. A caller that refused a token while a refresh was running
    /// takes that refresh's result instead of starting another, even when Codex returned the same bearer.
    private let refreshes = Counter()
    /// Asks for a refreshed token; ``refreshCodex(_:)``, replaced only by tests.
    var refresher: (() throws -> Token)?

    /// Validates `cfg`; the first use fetches.
    public init(_ cfg: CodexAuthConfiguration) throws {
        guard cfg.margin > 0, cfg.timeout > 0, cfg.margin.isFinite, cfg.timeout.isFinite else {
            throw CodexAuthConfigurationError("codexauth: margin and timeout are required, finite and positive")
        }
        self.cfg = cfg
        env = cfg.environment ?? ProcessInfo.processInfo.environment
        home = cfg.home ?? NSHomeDirectory()
        // As in Go: an empty explicit home is no choice, so the inherited CODEX_HOME applies to the child and the
        // file alike.
        let explicit = (cfg.codexHome ?? "").isEmpty ? nil : cfg.codexHome
        let ch = explicit ?? env["CODEX_HOME"]
        codexHome = (ch?.isEmpty ?? true) ? nil : ch
        path = env["PATH"] ?? ""
        candidates = cfg.candidates ?? CodexExecutable.candidates(home: home)
    }

    public var description: String { "Credentials{\(cfg.source.rawValue)}" }
    public var customMirror: Mirror { Mirror(self, children: ["source": cfg.source.rawValue], displayStyle: .class) }

    /// What the latest fetch found.
    public func info() async -> CodexAuthInfo {
        await withCheckedContinuation { cont in queue.async { cont.resume(returning: self.latest) } }
    }

    /// A fresh bearer, fetching on first use and refreshing when less than the margin remains. A token from auth.json
    /// is re-read rather than refreshed.
    public func token() async throws -> Token {
        try await run { op in try self.tokenLocked(op) }
    }

    /// Called when a request made with `stale` was refused as auth-expired. When `stale` was already replaced by a
    /// fresh token, or a refresh completed while this caller waited, that token is returned without another refresh;
    /// otherwise a refresh runs, within the backoff. Until a refresh succeeds, ``token()`` does not hand out `stale`.
    public func refresh(after stale: Token) async throws -> Token {
        let seen = refreshes.value
        return try await run { op in try self.refreshAfterLocked(stale, seen: seen, op) }
    }

    /// Runs `body` on the queue. A task cancelled while it waits its turn is released at once and its body never
    /// runs; one cancelled while the body runs ends the Codex call in progress, and the failure that produces keeps
    /// its state but has the class cancelled (spec/credentials.md).
    private func run<T: Sendable>(_ body: @escaping @Sendable (Operation) throws -> T) async throws -> T {
        let op = Operation()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<T, Error>) in
                guard op.install({ cont.resume(with: $0.map { $0 as! T }) }) else { return }
                queue.async {
                    guard op.begin() else { return }
                    op.finish(Result { try body(op) }.mapError { op.isCancelled ? Self.interrupted($0) : $0 })
                }
            }
        } onCancel: {
            op.cancel()
        }
    }

    /// `error` as the caller's cancellation left it: an interrupt is never reported as a credential failure.
    private static func interrupted(_ error: Error) -> Error {
        guard var e = error as? CodexAuthError else { return error }
        e.errorClass = .cancelled
        return e
    }

    /// Installs `tok` as if a fetch had returned it; for tests, with ``refresher``.
    func seedForTesting(_ tok: Token, refreshable: Bool) {
        queue.sync {
            current = tok
            self.refreshable = refreshable
        }
    }

    // MARK: - On the queue

    private func fresh(_ t: Token) -> Bool {
        guard let expires = t.expires else { return true }  // unknown expiry: the provider decides
        return expires.timeIntervalSince(cfg.now()) >= cfg.margin
    }

    /// Whether `t` may be handed out: fresh, and not the bearer a provider refused.
    private func usable(_ t: Token) -> Bool { fresh(t) && !sameBearer(t.bearer, refused) }

    private func tokenLocked(_ op: Operation) throws -> Token {
        if current == nil || (!refreshable && !usable(current!)) { try fetchLocked(op) }
        if usable(current!) { return current! }
        return try refreshLocked(op)
    }

    private func refreshAfterLocked(_ stale: Token, seen: Int, _ op: Operation) throws -> Token {
        // A refresh ran while this caller waited, and its token has not been refused since.
        if let current, refreshes.value != seen, usable(current) { return current }
        if let current, !sameBearer(current.bearer, stale.bearer) {
            if usable(current) { return current }
            // stale is an older token. The current one is refused or within the margin already; recording stale
            // would forget that the current one was refused.
        } else {
            refused = stale.bearer
        }
        if current == nil || !refreshable {
            try fetchLocked(op)
            if usable(current!) { return current! }
            if !refreshable {
                throw CodexAuthError(
                    .expired, message: "auth.json holds the refused token, and the file source cannot refresh")
            }
        }
        return try refreshLocked(op)
    }

    private func refreshLocked(_ op: Operation) throws -> Token {
        guard refreshable else {
            throw CodexAuthError(
                .expired, message: "the token in auth.json is expiring, and the file source cannot refresh")
        }
        if let retryAt, cfg.now() < retryAt {
            cfg.log?("codexauth: refresh held back by the backoff (until \(Self.stamp(retryAt)))")
            var e = CodexAuthError(
                .expired, message: "the last refresh failed; the next attempt waits for the backoff", cause: lastFailure)
            e.retryAt = retryAt
            e.capturedStderr = lastStderr
            throw e
        }
        do {
            let tok = try (refresher ?? { try self.refreshCodex(op) })()
            // auth.json, read because Codex lost getAuthStatus: it cannot be refreshed again.
            let fromFile = tok.source == "file"
            guard fresh(tok) else {
                throw CodexAuthConfigurationError("Codex returned a token that is still within the margin")
            }
            if fromFile, sameBearer(tok.bearer, refused) {
                throw CodexAuthConfigurationError("auth.json holds the refused token, and the file source cannot refresh")
            }
            backoff = 0
            retryAt = nil
            lastFailure = ""
            lastStderr = ""
            refused = nil
            cfg.log?("codexauth: refreshed the token")
            current = tok
            if fromFile { refreshable = false }  // with the token it applies to, once that is accepted
            refreshes.increment()
            return tok
        } catch {
            if op.isCancelled {
                // The caller gave up; Codex did not fail, so no backoff is spent.
                cfg.log?("codexauth: refresh cancelled; no backoff spent")
                var e = CodexAuthError(.expired, message: "the refresh was cancelled", cause: String(describing: error))
                e.capturedStderr = Self.stderr(of: error)
                throw e
            }
            backoff = backoff == 0 ? Self.backoffStart : min(2 * backoff, Self.backoffMax)
            retryAt = cfg.now().addingTimeInterval(backoff)  // from the failure, so a slow attempt keeps the wait
            lastFailure = String(describing: error)
            lastStderr = Self.stderr(of: error)
            cfg.log?(
                "codexauth: refresh failed (state \(CodexAuthError.state(of: error).rawValue), backoff \(Int(backoff)) s, "
                    + "until \(Self.stamp(retryAt!)))")
            // The refresh reply named another sign-in: classified as in step 3 (spec/credentials.md), not expired.
            if var e = error as? CodexAuthError,
                [.notSignedIn, .apiKey, .unsupportedAuth, .providerWithoutOpenAIAuth].contains(e.state)
            {
                e.retryAt = retryAt
                throw e
            }
            var failed = CodexAuthError(.expired, message: "the refresh did not renew the sign-in", cause: lastFailure)
            failed.retryAt = retryAt
            failed.capturedStderr = lastStderr
            throw failed
        }
    }

    private func fetchLocked(_ op: Operation) throws {
        do {
            let (tok, canRefresh) = try fetch(op)
            current = tok
            refreshable = canRefresh
            cfg.log?("codexauth: fetched a token (source \(latest.source?.rawValue ?? ""), refreshable \(canRefresh))")
        } catch var e as CodexAuthError {
            e.codexVersion = latest.codexVersion
            let cls = op.isCancelled ? ErrorClass.cancelled : e.errorClass
            cfg.log?(
                "codexauth: fetch failed (source \(latest.source?.rawValue ?? ""), state \(e.state.rawValue), "
                    + "class \(cls.rawValue))")
            throw e
        }
    }

    /// A time as the debug lines print it: RFC 3339, in UTC.
    private static func stamp(_ t: Date) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.string(from: t)
    }

    private func fetch(_ op: Operation) throws -> (Token, Bool) {
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
        return try codexToken(bin, op)
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
        let loc: AuthLocation
        do { loc = try AuthFile.location(codexHome: codexHome, home: home, confine: cfg.confineCodexHome) } catch let e
            as CodexAuthError
        { throw fallbackState(e) }
        defer { loc.close() }
        if let guardAccess = cfg.guardAccess {
            do { try guardAccess(loc.path) } catch {
                throw CodexAuthError(
                    .codexError, message: "reading auth.json was refused", cause: String(describing: error))
            }
        }
        do { return try AuthFile.read(loc, onRetry: cfg.onFileRetry) } catch let e as CodexAuthError {
            throw fallbackState(e)
        }
    }

    private func start(_ bin: String, _ op: Operation) throws -> AppServerClient {
        if op.isCancelled { throw CodexAuthError(.codexError, message: "the call was cancelled") }
        let client: AppServerClient
        do {
            // Attached as soon as it is spawned, so a cancellation ends the initialize handshake too.
            client = try AppServerClient.start(
                .init(
                    bin: bin, codexHome: codexHome, environment: env, timeout: cfg.timeout, clientName: cfg.clientName,
                    guardStart: cfg.guardAccess, log: cfg.log),
                onSpawn: { op.attach($0) })
        } catch {
            var e = CodexAuthError(
                .codexError, message: "Codex app-server did not start", cause: String(describing: error))
            e.capturedStderr = Self.stderr(of: error)
            throw e
        }
        let version = Self.version(client.userAgent)
        latest.codexVersion = version
        if !version.isEmpty, CodexExecutable.versionLess(version, minimumCodexVersion) {
            client.close()
            throw CodexAuthError(.codexTooOld, codexVersion: version, message: "Codex is older than \(minimumCodexVersion)")
        }
        return client
    }

    /// The stderr of a failed Codex start that `error` carries, "" for none: Go finds it through the cause chain.
    private static func stderr(of error: Error) -> String {
        (error as? CodexAuthError)?.stderr ?? (error as? AppServerError)?.stderr ?? ""
    }

    /// Asks a fresh app-server child: metadata first, and the token only for a ChatGPT sign-in, so an API key is
    /// never requested.
    private func codexToken(_ bin: String, _ op: Operation) throws -> (Token, Bool) {
        latest.source = .codex
        let client = try start(bin, op)
        defer { client.close() }
        let meta: AuthStatus
        do { meta = try client.getAuthStatus(includeToken: false, refreshToken: false) } catch let e as AppServerError
            where e.rpcCode == AppServerError.methodNotFound
        {
            client.close()
            return (try storeFallback(bin, op), false)
        } catch {
            throw CodexAuthError(
                .codexError, message: "Codex app-server getAuthStatus failed", cause: String(describing: error))
        }
        guard meta.requiresOpenAIAuth else {
            throw CodexAuthError(.providerWithoutOpenAIAuth, message: "the provider in force needs no OpenAI sign-in")
        }
        if let e = classifyMode(meta.method) { throw e }
        let full: AuthStatus
        do { full = try client.getAuthStatus(includeToken: true, refreshToken: false) } catch let e as AppServerError
            where e.rpcCode == AppServerError.methodNotFound
        {
            client.close()
            return (try storeFallback(bin, op), false)
        } catch {
            throw CodexAuthError(
                .codexError, message: "Codex app-server getAuthStatus failed", cause: String(describing: error))
        }
        return (try Self.token(from: full), true)
    }

    /// Runs when Codex lacks getAuthStatus: a fresh child's config/read decides whether auth.json is Codex's
    /// authority. A running app-server keeps the store it started with, so the answer comes from a new child.
    private func storeFallback(_ bin: String, _ op: Operation) throws -> Token {
        let client = try start(bin, op)
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
    private func refreshCodex(_ op: Operation) throws -> Token {
        guard let bin = CodexExecutable.resolve(explicit: cfg.executable, path: path, candidates: candidates) else {
            throw CodexAuthError(.noCodex, message: "no Codex executable resolved")
        }
        let client = try start(bin, op)
        defer { client.close() }
        let full: AuthStatus
        do { full = try client.getAuthStatus(includeToken: true, refreshToken: true) } catch let e as AppServerError
            where e.rpcCode == AppServerError.methodNotFound
        {
            // Codex was updated to one without getAuthStatus since the token was fetched: the same config/read rule
            // decides, and a file token cannot be refreshed again, which refreshLocked records once it accepts it.
            client.close()
            return try storeFallback(bin, op)
        } catch {
            throw CodexAuthError(
                .codexError, message: "Codex app-server getAuthStatus failed", cause: String(describing: error))
        }
        return try Self.token(from: full)
    }

    /// Checks the provider and the method again on a token reply: a change is classified like the metadata reply.
    private static func token(from full: AuthStatus) throws -> Token {
        guard full.requiresOpenAIAuth else {
            throw CodexAuthError(.providerWithoutOpenAIAuth, message: "the provider in force needs no OpenAI sign-in")
        }
        if let e = classifyMode(full.method) { throw e }
        guard !full.token.isEmpty else {
            throw CodexAuthError(.codexError, message: "Codex reported a ChatGPT sign-in without a token")
        }
        return makeToken(full.token, source: "codex")
    }

    /// The Codex version in an initialize userAgent such as "codex_cli_rs/0.162.1 (Mac OS 15.0; arm64)"; "" when
    /// there is none.
    /// Read as bytes, as Go reads it: a digit followed by a combining mark is one Character but still a digit.
    public static func version(_ userAgent: String) -> String {
        let bytes = Array(userAgent.utf8)
        guard let slash = bytes.firstIndex(of: 0x2F) else { return "" }
        let rest = bytes[(slash + 1)...].prefix { $0 == 0x2E || ($0 >= 0x30 && $0 <= 0x39) }
        guard let first = rest.first, first != 0x2E else { return "" }
        return String(decoding: rest, as: UTF8.self)
    }
}

/// One call into ``Credentials``: whether it was cancelled, how to release its caller, and the Codex client it is
/// using, so a cancellation can end that client's call.
final class Operation: @unchecked Sendable {
    private let lock = NSLock()
    private var resume: ((Result<any Sendable, Error>) -> Void)?
    private var cancelled = false
    private var running = false
    private var done = false
    private var client: AppServerClient?

    var isCancelled: Bool { lock.withLock { cancelled } }

    /// Installs the continuation; false when the task was already cancelled, in which case it is resumed here.
    func install(_ resume: @escaping (Result<any Sendable, Error>) -> Void) -> Bool {
        let early = lock.withLock { () -> Bool in
            if cancelled {
                done = true
                return true
            }
            self.resume = resume
            return false
        }
        if early { resume(.failure(CancellationError())) }
        return !early
    }

    /// Called on the queue before the body; false when the caller was released while waiting.
    func begin() -> Bool {
        lock.withLock {
            guard !done, !cancelled else { return false }
            running = true
            return true
        }
    }

    func finish<T: Sendable>(_ result: Result<T, Error>) {
        let r = lock.withLock { () -> ((Result<any Sendable, Error>) -> Void)? in
            guard !done else { return nil }
            done = true
            client = nil
            defer { resume = nil }
            return resume
        }
        r?(result.map { $0 as any Sendable })
    }

    func cancel() {
        let (release, active) = lock.withLock { () -> (((Result<any Sendable, Error>) -> Void)?, AppServerClient?) in
            cancelled = true
            if !running && !done {
                done = true
                defer { resume = nil }
                return (resume, nil)
            }
            return (nil, done ? nil : client)
        }
        release?(.failure(CancellationError()))
        active?.cancel()
    }

    /// The client the body now uses; cancelled at once when the operation already was.
    func attach(_ c: AppServerClient) {
        let cancelNow = lock.withLock { () -> Bool in
            client = c
            return cancelled
        }
        if cancelNow { c.cancel() }
    }
}

/// A counter read from any thread.
final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    var value: Int { lock.withLock { n } }
    func increment() { lock.withLock { n += 1 } }
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
            var e = (error as? CodexAuthError) ?? CodexAuthError(.codexError, cause: String(describing: error))
            if error is CancellationError {  // cancelled while it waited its turn, as Go's context error
                e.message = "the call was cancelled"
                e.errorClass = .cancelled
            }
            return CodexAuthStatus(
                state: e.state, info: await c.info(), mode: e.mode, expires: nil, error: e, credentials: c)
        }
    }
}
/// Whether two bearers are the same bytes, as Go compares them: Swift's `==` treats canonically equivalent strings as
/// equal, and two different credentials can be (`tok-\u{E9}`, `tok-e\u{301}`).
func sameBearer(_ a: String?, _ b: String?) -> Bool {
    switch (a, b) {
    case (nil, nil): return true
    case let (x?, y?): return x.utf8.elementsEqual(y.utf8)
    default: return false
    }
}
#endif
