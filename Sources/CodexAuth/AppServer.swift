#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// A failure of the app-server child: it did not start, crashed, timed out, misbehaved, or answered a call with a
/// JSON-RPC error. `message` never quotes the child's output; `rpcMessage` is scrubbed and kept out of
/// `description`, because a misbehaving child may put a credential there.
public struct AppServerError: Error, Sendable, Equatable, CustomStringConvertible, CustomReflectable {
    /// The JSON-RPC code of an error reply; nil for any other failure.
    public let rpcCode: Int?
    public let rpcMessage: String
    public let message: String

    /// Both strings are scrubbed.
    public init(_ message: String, rpcCode: Int? = nil, rpcMessage: String = "") {
        self.message = Secret.scrub(message)
        self.rpcCode = rpcCode
        self.rpcMessage = Secret.scrub(rpcMessage)
    }

    /// The JSON-RPC code for an unknown method.
    public static let methodNotFound = -32601

    /// Scrubbed whole when formatted, so a secret registered since is removed too.
    public var description: String {
        Secret.scrub(rpcCode.map { "\(message): JSON-RPC error \($0)" } ?? message)
    }

    /// Reflection shows what `description` shows; `rpcMessage` stays out, as there.
    public var customMirror: Mirror {
        Mirror(self, children: ["rpcCode": rpcCode as Any, "message": Secret.scrub(message)], displayStyle: .struct)
    }
}

/// The getAuthStatus result. The token is registered with the scrubber and redacted by every formatting path,
/// reflection (`dump`, `Mirror`) included.
public struct AuthStatus: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible,
    CustomReflectable
{
    /// `authMethod`; nil when Codex reports null or nothing (not signed in). An empty string is a mode, and not
    /// chatgpt.
    public let method: String?
    /// `requiresOpenaiAuth`; false means a provider needing no OpenAI sign-in is in force.
    public let requiresOpenAIAuth: Bool
    /// The credential Codex sent, or "".
    public let token: String

    public init(method: String?, requiresOpenAIAuth: Bool, token: String) {
        Secret.register(token)
        self.method = method
        self.requiresOpenAIAuth = requiresOpenAIAuth
        self.token = token
    }

    /// By bytes, as Go compares them: Swift's `==` treats canonically equivalent strings as equal, and two different
    /// credentials can be (`tok-\u{E9}`, `tok-e\u{301}`).
    public static func == (a: AuthStatus, b: AuthStatus) -> Bool {
        let sameMethod: Bool
        switch (a.method, b.method) {
        case (nil, nil): sameMethod = true
        case let (x?, y?): sameMethod = x.utf8.elementsEqual(y.utf8)
        default: sameMethod = false
        }
        return sameMethod && a.requiresOpenAIAuth == b.requiresOpenAIAuth && a.token.utf8.elementsEqual(b.token.utf8)
    }

    public var description: String { "AuthStatus{method: \(Secret.scrub(method ?? ""))}" }
    public var debugDescription: String { description }
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "method": Secret.scrub(method ?? ""), "requiresOpenAIAuth": requiresOpenAIAuth, "token": Secret.redacted,
            ], displayStyle: .struct)
    }
}

/// A message from the child that is not a reply: a notification, or a server request, which the client has already
/// answered with an error (``isServerRequest``). Formatting and reflection show the kind, the method scrubbed and
/// escaped (so a control character in it cannot forge a log line or reach a terminal) and the size of ``params``,
/// never the params themselves: a notification may carry what the user said, or a credential.
public struct AppServerEvent: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible,
    CustomReflectable
{
    public let method: String
    /// The params member exactly as the child wrote it, strict JSON; empty when absent (a `"params": null` is
    /// `null`).
    public let params: Data
    /// Set for a server request (it had an id), which was refused before the event was queued.
    public let isServerRequest: Bool

    public init(method: String, params: Data, isServerRequest: Bool) {
        self.method = method
        self.params = params
        self.isServerRequest = isServerRequest
    }

    /// ``params`` decoded as a JSON object; nil when absent or not an object.
    public func paramsObject() -> [String: Any]? {
        guard !params.isEmpty else { return nil }
        return (try? JSONSerialization.jsonObject(with: params, options: [.fragmentsAllowed])) as? [String: Any]
    }

    /// By bytes, as Go compares them.
    public static func == (a: AppServerEvent, b: AppServerEvent) -> Bool {
        a.method.utf8.elementsEqual(b.method.utf8) && a.params == b.params && a.isServerRequest == b.isServerRequest
    }

    public var description: String {
        "AppServerEvent{\(isServerRequest ? "server request" : "notification") \(Self.quoted(Secret.scrub(method))), \(params.count) bytes}"
    }
    public var debugDescription: String { description }
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "method": Self.quoted(Secret.scrub(method)), "params": "\(params.count) bytes",
                "isServerRequest": isServerRequest,
            ], displayStyle: .struct)
    }

    /// `s` in double quotes, with a quote, a backslash and every scalar that is not printable (controls, format
    /// characters such as a bidirectional override, separators other than the space) escaped, as Go's strconv.Quote
    /// writes them.
    static func quoted(_ s: String) -> String {
        var out = "\""
        for u in s.unicodeScalars {
            switch u {
            case "\"": out += "\\\""
            case "\\": out += "\\\\"
            case "\u{07}": out += "\\a"
            case "\u{08}": out += "\\b"
            case "\u{0C}": out += "\\f"
            case "\n": out += "\\n"
            case "\r": out += "\\r"
            case "\t": out += "\\t"
            case "\u{0B}": out += "\\v"
            default:
                if u.value < 0x20 || u.value == 0x7F {
                    out += "\\x" + String(format: "%02x", u.value)
                    continue
                }
                switch u.properties.generalCategory {
                case .control, .format, .lineSeparator, .paragraphSeparator, .surrogate, .unassigned, .privateUse,
                    .spaceSeparator where u != " ":
                    out += u.value <= 0xFFFF
                        ? "\\u" + String(format: "%04x", u.value) : "\\U" + String(format: "%08x", u.value)
                default: out.unicodeScalars.append(u)
                }
            }
        }
        return out + "\""
    }
}

/// One `codex app-server --listen stdio://` child, alive until ``close()`` or until the client is released,
/// speaking newline-delimited JSON-RPC (spec/credentials.md, the app-server source). Its calls block the calling
/// thread up to the per-call timeout, so call them off the main thread and off Swift's cooperative pool.
public final class AppServerClient: @unchecked Sendable {
    /// How to start the child. Formatting and reflection never show the environment, which may hold credentials.
    public struct Configuration: Sendable, CustomStringConvertible, CustomReflectable {
        /// An absolute path, from ``CodexExecutable/resolve(explicit:path:candidates:)``.
        public var bin: String
        /// Passed as `CODEX_HOME` when set.
        public var codexHome: String?
        /// The inherited environment.
        public var environment: [String: String]
        /// Per call; required.
        public var timeout: TimeInterval
        /// Sent as `clientInfo.name`.
        public var clientName: String
        /// Runs before the executable is started; a throw refuses the start. Keeps unit tests away from a real Codex.
        public var guardStart: (@Sendable (String) throws -> Void)?
        /// Receives debug lines, which never hold the child's output.
        public var log: (@Sendable (String) -> Void)?
        /// The child's working directory: an absolute path to an existing folder. nil keeps the caller's.
        public var workingDirectory: String?
        /// Sends `capabilities.experimentalApi: true` with initialize.
        public var experimentalAPI: Bool
        /// When set, receives every notification and every server request (already refused), in the order the child
        /// wrote them, one at a time on a thread of its own: the reader never waits for it. A reply may reach its
        /// caller before the handler has seen the events written before it. Events held beyond 8 MiB (each charged
        /// its method and params bytes plus 64) or beyond 65,536 of them, the one being handled included, fail the
        /// client. After a failure the events already queued are still delivered; after ``AppServerClient/close()``,
        /// none that has not started. nil ignores notifications.
        public var onEvent: (@Sendable (AppServerEvent) -> Void)?

        public init(
            bin: String, codexHome: String? = nil, environment: [String: String] = ProcessInfo.processInfo.environment,
            timeout: TimeInterval, clientName: String = "sayso", guardStart: (@Sendable (String) throws -> Void)? = nil,
            log: (@Sendable (String) -> Void)? = nil, workingDirectory: String? = nil, experimentalAPI: Bool = false,
            onEvent: (@Sendable (AppServerEvent) -> Void)? = nil
        ) {
            self.bin = bin
            self.codexHome = codexHome
            self.environment = environment
            self.timeout = timeout
            self.clientName = clientName
            self.guardStart = guardStart
            self.log = log
            self.workingDirectory = workingDirectory
            self.experimentalAPI = experimentalAPI
            self.onEvent = onEvent
        }

        public var description: String { "AppServerClient.Configuration{\(bin)}" }
        public var customMirror: Mirror {
            Mirror(self, children: ["bin": bin, "environment": Secret.redacted], displayStyle: .struct)
        }
    }

    /// The child's arguments. The provider override makes Codex answer for its OpenAI sign-in even when the user's
    /// configured provider needs none.
    public static let arguments = ["-c", #"model_provider="openai""#, "app-server", "--listen", "stdio://"]

    static let killGrace: TimeInterval = 2
    static let reapGrace: TimeInterval = 1
    static let stderrCap = 64 << 10
    static let maxLine = 4 << 20
    /// How long close waits, once the child is gone, for its last stderr bytes.
    static let stderrDrain: TimeInterval = 0.1

    /// The child's process ID.
    public var pid: pid_t { core.pid }
    /// The initialize `userAgent`, scrubbed; "" when absent.
    public private(set) var userAgent = ""

    /// The child and its readers. The reader threads hold the core, never the client, so releasing an unclosed
    /// client runs its deinit, which ends the child.
    private let core: AppServerCore

    private init(core: AppServerCore) { self.core = core }

    /// Spawns the child in its own process group and completes the initialize handshake.
    public static func start(_ cfg: Configuration) throws -> AppServerClient { try start(cfg, onSpawn: nil) }

    /// As ``start(_:)``; `onSpawn` sees the client before the handshake, so a caller can cancel the handshake.
    static func start(_ cfg: Configuration, onSpawn: ((AppServerClient) -> Void)?) throws -> AppServerClient {
        guard cfg.bin.utf8.first == 0x2F, !cfg.bin.utf8.contains(0) else {
            throw AppServerError("the Codex executable path is not absolute")
        }
        guard cfg.timeout > 0, cfg.timeout.isFinite else { throw AppServerError("a per-call timeout is required") }
        if let dir = cfg.workingDirectory {
            // The path is not quoted: a caller's folder name is not ours to print.
            var st = stat()
            guard dir.utf8.first == 0x2F, !dir.utf8.contains(0), stat(dir, &st) == 0,
                st.st_mode & mode_t(S_IFMT) == mode_t(S_IFDIR)
            else { throw AppServerError("the working directory is not an absolute path to an existing folder") }
        }
        // A name holding "=" would become another variable in the child ("OPENAI_API_KEY=x" with value "y" is
        // OPENAI_API_KEY=x=y) and slip past the removal list; a NUL would cut the string short. Go cannot be given
        // either: its entries are "name=value" strings, and exec refuses a NUL.
        let passable = cfg.environment.allSatisfy { name, value in
            !name.utf8.contains(0x3D) && !name.utf8.contains(0) && !value.utf8.contains(0)
        }
        guard passable, !(cfg.codexHome?.utf8.contains(0) ?? false)
        else { throw AppServerError("the environment holds a name with \"=\" or a NUL, which cannot be passed on") }
        if let guardStart = cfg.guardStart {
            do { try guardStart(cfg.bin) } catch {
                throw AppServerError("starting Codex was refused: \(Secret.scrub(String(describing: error)))")
            }
        }
        let env = CodexExecutable.childEnvironment(cfg.environment, bin: cfg.bin, codexHome: cfg.codexHome)
        let c = AppServerClient(
            core: try AppServerCore(
                bin: cfg.bin, env: env, timeout: cfg.timeout, log: cfg.log, directory: cfg.workingDirectory,
                onEvent: cfg.onEvent))
        onSpawn?(c)
        do {
            var capabilities: [String: Any] = [:]
            if cfg.experimentalAPI { capabilities["experimentalApi"] = true }
            let hello = try c.core.call(
                "initialize",
                params: ["clientInfo": ["name": cfg.clientName, "version": "0"], "capabilities": capabilities])
            // A null result reads as an empty one, as in Go.
            guard let object = object(hello), let agent = optionalString(object["userAgent"]) else {
                throw AppServerError("Codex app-server sent an unexpected initialize result")
            }
            c.userAgent = Secret.scrub(agent ?? "")
            try c.core.notify(["method": "initialized"])
        } catch {
            c.close()
            throw error
        }
        return c
    }

    deinit { core.close() }

    /// The first 64 KiB the child wrote to stderr, scrubbed. It is for diagnostics a caller chooses to show; the
    /// package never logs it, because the scrubber cannot recognise every credential format.
    public var stderr: String { core.stderr }

    /// Sends one request and returns its `result`, which is NSNull when null. Fails after the per-call timeout, on a
    /// crash, on malformed output, or when the reply has neither result nor error; a JSON-RPC error reply carries
    /// its code.
    public func call(_ method: String, params: [String: Any] = [:]) throws -> Any {
        try core.call(method, params: params)
    }

    /// getAuthStatus. Any token in the reply is registered with the scrubber before it is returned.
    public func getAuthStatus(includeToken: Bool, refreshToken: Bool) throws -> AuthStatus {
        let result = try call("getAuthStatus", params: ["includeToken": includeToken, "refreshToken": refreshToken])
        let bad = AppServerError("Codex app-server sent an unexpected getAuthStatus result")
        guard let o = object(result) else { throw bad }
        guard let token = optionalString(o["authToken"]) else { throw bad }
        Secret.register(token ?? "")  // before anything else can fail or format it
        guard let method = optionalString(o["authMethod"]) else { throw bad }
        var requires = true
        if let r = o["requiresOpenaiAuth"], !(r is NSNull) {
            guard let b = bool(r) else { throw bad }
            requires = b
        }
        return AuthStatus(method: method, requiresOpenAIAuth: requires, token: token ?? "")
    }

    /// config/read's effective `cli_auth_credentials_store`; "" when absent, which Codex treats as "file".
    public func credentialsStore() throws -> String {
        let result = try call("config/read", params: ["includeLayers": false])
        let bad = AppServerError("Codex app-server sent an unexpected config/read result")
        guard let o = object(result) else { throw bad }
        var config: [String: Any] = [:]
        if let c = o["config"], !(c is NSNull) {
            guard let c = c as? [String: Any] else { throw bad }
            config = c
        }
        guard let store = optionalString(config["cli_auth_credentials_store"]) else { throw bad }
        return Secret.scrub(store ?? "")
    }

    /// account/read's `account.planType`, "" when absent or null. Only this field is read; a present field of the
    /// wrong type is an error, as in Go. account/read may reach the network.
    public func planType() throws -> String {
        let result = try call("account/read")
        let bad = AppServerError("Codex app-server sent an unexpected account/read result")
        guard let o = object(result) else { throw bad }
        guard let a = o["account"], !(a is NSNull) else { return "" }
        guard let account = a as? [String: Any], let plan = optionalString(account["planType"]) else { throw bad }
        return Secret.scrub(plan ?? "")
    }

    /// Ends the child: stdin closed, SIGTERM to its process group, SIGKILL to the group after 2 s when the child or
    /// any other member of its group is still alive. Returns within grace + 1 s even when a descendant outside the
    /// group still holds the pipes: the readers are abandoned and close their ends. Safe to call more than once.
    public func close() { core.close() }

    /// Ends every call in progress with a failure; for a caller that gave up. The client is unusable afterwards.
    func cancel() { core.fail(AppServerError("Codex app-server call was cancelled")) }

    /// Whether the child has been reaped.
    var hasExited: Bool { core.hasExited }
}

/// The child's process, pipes, readers and pending calls.
final class AppServerCore: @unchecked Sendable {
    /// Marks a reply with neither result nor error, which ``call(_:params:)`` reports as an unexpected result.
    private static let missingResult = "a reply without a result"

    let pid: pid_t
    private let timeout: TimeInterval
    private let log: (@Sendable (String) -> Void)?
    private let stdinFD: Int32
    private let lock = NSLock()
    /// Admits one writer at a time; a semaphore, so a waiting writer observes its deadline and a failure.
    private let writeGate = DispatchSemaphore(value: 1)
    /// Signalled when the stderr reader ends.
    private let stderrDone = DispatchSemaphore(value: 0)
    private var nextID = 0
    private var pending: [Int: Pending] = [:]
    private var failure: AppServerError?
    private var stopReaders = false
    /// Set when close starts; `closed` follows once the queued refusals are written, and then stops every writer.
    private var closeStarted = false
    private var closed = false
    private var stdinOpen = true  // guarded by writeGate
    private var stderrBytes: [UInt8] = []
    /// The encoded answers to server requests, written in order by ``refuseLoop()`` so the reader never waits on the
    /// child's input; `refusalBytes` counts them until written, at most `refusalCap`. Guarded by `lock`.
    private var refusals: [Data] = []
    private var refusalBytes = 0
    /// Count the refusals queued and written, so a request waits for the answers queued before it and no later ones.
    private var refusalsQueued = 0
    private var refusalsDone = 0
    private let refusalWake = DispatchSemaphore(value: 0)
    /// Bounds the refusals queued for a child that sends server requests without reading the answers.
    private static let refusalCap = 2 * AppServerClient.maxLine
    /// Bounds how long close waits for queued refusals to be written before it closes stdin.
    private static let refusalFlush: TimeInterval = 0.1

    /// The events waiting for `onEvent`, from `eventHead` on, delivered in order by ``eventLoop()``; `eventBytes`
    /// and `eventCount` are what they are charged, the one being delivered included, at most `eventCap` and
    /// `maxEvents`. Guarded by `lock`.
    private let onEvent: (@Sendable (AppServerEvent) -> Void)?
    private var events: [AppServerEvent] = []
    private var eventHead = 0
    private var eventBytes = 0
    private var eventCount = 0
    private let eventWake = DispatchSemaphore(value: 0)
    /// Bounds the events held for a handler that does not keep up.
    private static let eventCap = 2 * AppServerClient.maxLine
    /// What each held event is charged on top of its method and params bytes, so a flood of tiny events reaches
    /// `eventCap` too.
    private static let eventOverhead = 64
    /// Bounds the number of events held, whatever their size.
    private static let maxEvents = 1 << 16
    private let exited = DispatchSemaphore(value: 0)
    /// Signalled when the first close has finished.
    private let closeDone = DispatchSemaphore(value: 0)

    private final class Pending {
        let done = DispatchSemaphore(value: 0)
        var result: Result<Any, AppServerError>?
    }

    init(
        bin: String, env: [String: String], timeout: TimeInterval, log: (@Sendable (String) -> Void)?,
        directory: String?, onEvent: (@Sendable (AppServerEvent) -> Void)?
    ) throws {
        self.timeout = timeout
        self.log = log
        self.onEvent = onEvent
        var inPipe: [Int32] = [-1, -1], outPipe: [Int32] = [-1, -1], errPipe: [Int32] = [-1, -1]
        guard pipe(&inPipe) == 0 else { throw AppServerError("Codex app-server could not be started") }
        guard pipe(&outPipe) == 0 else {
            Self.closeAll(inPipe)
            throw AppServerError("Codex app-server could not be started")
        }
        guard pipe(&errPipe) == 0 else {
            Self.closeAll(inPipe + outPipe)
            throw AppServerError("Codex app-server could not be started")
        }
        for fd in inPipe + outPipe + errPipe { _ = fcntl(fd, F_SETFD, FD_CLOEXEC) }
        _ = fcntl(inPipe[1], F_SETNOSIGPIPE, 1)
        // Our end only: a write that cannot proceed waits in poll, against the call's deadline.
        _ = fcntl(inPipe[1], F_SETFL, fcntl(inPipe[1], F_GETFL) | O_NONBLOCK)

        var actions: posix_spawn_file_actions_t?
        var attr: posix_spawnattr_t?
        posix_spawn_file_actions_init(&actions)
        posix_spawnattr_init(&attr)
        defer {
            posix_spawn_file_actions_destroy(&actions)
            posix_spawnattr_destroy(&attr)
        }
        posix_spawn_file_actions_adddup2(&actions, inPipe[0], 0)
        posix_spawn_file_actions_adddup2(&actions, outPipe[1], 1)
        posix_spawn_file_actions_adddup2(&actions, errPipe[1], 2)
        if let directory { posix_spawn_file_actions_addchdir_np(&actions, directory) }
        // Its own process group, so close can signal every process the child starts; only fds 0-2 are inherited.
        var defaults = sigset_t()
        sigfillset(&defaults)
        var mask = sigset_t()
        sigemptyset(&mask)
        posix_spawnattr_setsigdefault(&attr, &defaults)
        posix_spawnattr_setsigmask(&attr, &mask)
        posix_spawnattr_setpgroup(&attr, 0)
        posix_spawnattr_setflags(
            &attr, Int16(POSIX_SPAWN_SETPGROUP | POSIX_SPAWN_CLOEXEC_DEFAULT | POSIX_SPAWN_SETSIGDEF
                | POSIX_SPAWN_SETSIGMASK))

        let argv = [bin] + AppServerClient.arguments
        let envp = env.map { "\($0.key)=\($0.value)" }
        var child: pid_t = 0
        let rc = Self.withCStrings(argv) { a in
            Self.withCStrings(envp) { e in posix_spawn(&child, bin, &actions, &attr, a, e) }
        }
        Self.closeAll([inPipe[0], outPipe[1], errPipe[1]])  // the child holds its copies
        guard rc == 0 else {
            Self.closeAll([inPipe[1], outPipe[0], errPipe[0]])
            throw AppServerError("Codex app-server could not be started")
        }
        pid = child
        stdinFD = inPipe[1]
        let childPID = child

        let out = outPipe[0], err = errPipe[0]
        Self.thread { [self] in readLoop(out) }
        Self.thread { [self] in refuseLoop() }
        if onEvent != nil { Self.thread { [self] in eventLoop() } }
        Self.thread { [self] in
            stderrLoop(err)
            stderrDone.signal()
        }
        // Waits for the exit without reaping: a zombie keeps its process ID, and so its group ID, ours, so close can
        // signal the group without reaching one that reused the ID. close reaps it last.
        Self.thread { [self] in
            var info = siginfo_t()
            while waitid(P_PID, id_t(childPID), &info, WEXITED | WNOWAIT) == -1 && errno == EINTR {}
            exited.signal()
        }
    }

    var stderr: String {
        lock.withLock { Secret.scrub(String(decoding: stderrBytes, as: UTF8.self)) }
    }

    func call(_ method: String, params: [String: Any]) throws -> Any {
        let deadline = DispatchTime.now() + timeout
        let p = Pending()
        let id: Int = try lock.withLock {
            if let failure { throw failure }
            nextID += 1
            pending[nextID] = p
            return nextID
        }
        defer { _ = lock.withLock { pending.removeValue(forKey: id) } }
        awaitRefusals(until: deadline)  // the write below fails as the wait did, on the deadline or a failure
        try write(["id": id, "method": method, "params": params], deadline: deadline)
        if p.done.wait(timeout: deadline) == .timedOut {
            throw AppServerError("Codex app-server did not answer \(method) within \(timeout) s")
        }
        switch lock.withLock({ p.result }) {
        case .success(let result): return result
        case .failure(let e) where e.rpcCode != nil:
            throw AppServerError("Codex app-server rejected \(method)", rpcCode: e.rpcCode, rpcMessage: e.rpcMessage)
        case .failure(let e) where e.message == Self.missingResult:
            throw AppServerError("Codex app-server sent an unexpected \(method) result")
        case .failure(let e): throw e
        case nil: throw AppServerError("Codex app-server exited unexpectedly")
        }
    }

    /// Sends a notification, within the per-call timeout.
    func notify(_ message: [String: Any]) throws {
        try write(message, deadline: .now() + timeout)
    }

    /// Concurrent and later calls wait until the first has finished, as Go's sync.Once makes them.
    func close() {
        let first = lock.withLock { () -> Bool in
            defer { closeStarted = true }
            return !closeStarted
        }
        guard first else {
            closeDone.wait()
            closeDone.signal()
            return
        }
        defer { closeDone.signal() }
        awaitRefusals(until: .now() + Self.refusalFlush)
        lock.withLock { closed = true }
        // A writer waiting on a full pipe, or for its turn, sees `closed` within one poll interval and lets go.
        writeGate.wait()
        if stdinOpen {
            Darwin.close(stdinFD)
            stdinOpen = false
        }
        writeGate.signal()
        // Once the child has exited and its group is empty nothing is signalled. The child is not reaped until the end
        // of close, so its group ID cannot name another group meanwhile.
        if !gone() { _ = kill(-pid, SIGTERM) }
        if !waitGroup(until: .now() + AppServerClient.killGrace) {
            _ = kill(-pid, SIGKILL)  // the group was alive a moment ago, so its ID is still ours
            if !reaped(by: .now() + AppServerClient.reapGrace) {
                log?("codex app-server was not reaped within the grace; abandoning it (pid \(pid))")
            }
        }
        // The last stderr bytes the child wrote are kept, unless a descendant still holds the pipe.
        if stderrDone.wait(timeout: .now() + AppServerClient.stderrDrain) == .success { stderrDone.signal() }
        let n = lock.withLock { () -> Int in
            stopReaders = true
            return stderrBytes.count
        }
        fail(AppServerError("Codex app-server was closed"))
        // The group is no longer signalled: reap the child now, or once it exits if it was abandoned.
        let child = pid
        if reaped(by: .now()) {
            var status: Int32 = 0
            while waitpid(child, &status, 0) == -1 && errno == EINTR {}
        } else {
            let exited = exited
            Self.thread {
                exited.wait()
                var status: Int32 = 0
                while waitpid(child, &status, 0) == -1 && errno == EINTR {}
            }
        }
        if n > 0 { log?("codex app-server wrote to stderr (\(n) bytes)") }
    }

    /// Waits until every answer to a server request queued so far has been written, or `deadline` or a failure ends the
    /// wait. A request waits for them before it is written, so the child reads each answer before any request the
    /// client sent after the server request arrived, as when the reader wrote the answers itself; close waits for
    /// them, at most `refusalFlush`, so a request the child sent before close is answered rather than dropped with the
    /// queue. Answers queued after the wait began are not waited for, so a child that keeps sending requests cannot
    /// hold a call.
    private func awaitRefusals(until deadline: DispatchTime) {
        let target = lock.withLock { refusalsQueued }
        while lock.withLock({ refusalsDone < target && failure == nil }), DispatchTime.now() < deadline { usleep(1000) }
    }

    var hasExited: Bool { reaped(by: .now()) }

    /// Whether the child has exited by `deadline` (it stays unreaped until close); the semaphore keeps its signal for
    /// the next look.
    private func reaped(by deadline: DispatchTime) -> Bool {
        guard exited.wait(timeout: deadline) == .success else { return false }
        exited.signal()
        return true
    }

    /// Whether the child was reaped and no other member of its process group remains, by `deadline`. A group
    /// outlives its leader while a member lives, so the group ID still names our group while kill(-pid, 0) succeeds.
    private func waitGroup(until deadline: DispatchTime) -> Bool {
        while true {
            if gone() { return true }
            let now = DispatchTime.now()
            if now >= deadline { return false }
            let next = min(deadline, now + .milliseconds(20))
            // Exited, but a member may still hold the group: the probes that follow are paced, not spun. The clock is
            // read once, so the subtraction cannot underflow.
            if reaped(by: next) {
                let t = DispatchTime.now().uptimeNanoseconds
                if t < next.uptimeNanoseconds { usleep(useconds_t((next.uptimeNanoseconds - t) / 1000)) }
            }
        }
    }

    /// Whether the child has exited and no live member of its process group remains.
    private func gone() -> Bool { reaped(by: .now()) && !groupAlive() }

    /// While the leader is an unreaped zombie, kill(-pgid, 0) fails with EPERM once no live member remains (measured
    /// on macOS), and succeeds while one does.
    private func groupAlive() -> Bool { kill(-pid, 0) == 0 }

    // MARK: - Wire

    /// Writes one message, giving up at `deadline`, so a child that stops reading cannot hold a call, or the
    /// writer of refusals, past its timeout; ``close()`` ends the wait too.
    /// Waiting for another writer counts against the deadline. A write cut off after part of the message was sent
    /// leaves the stream unusable, so the client is failed. A message JSONSerialization cannot encode (a Date, NaN)
    /// is refused before it reaches the encoder, which would raise an Objective-C exception no `try` can catch.
    private func write(_ message: [String: Any], deadline: DispatchTime) throws {
        try write(Self.encode(message), deadline: deadline)
    }

    /// `message` as one line, or a throw when JSONSerialization cannot encode it.
    private static func encode(_ message: [String: Any]) throws -> Data {
        guard encodable(message),
            var data = try? JSONSerialization.data(withJSONObject: message, options: [.withoutEscapingSlashes])
        else { throw AppServerError("a request could not be encoded") }
        data.append(0x0A)
        return data
    }

    /// Writes one encoded line, as the overload taking a message describes.
    private func write(_ data: Data, deadline: DispatchTime) throws {
        let refused = AppServerError("Codex app-server is not accepting requests")
        let timedOut = AppServerError("Codex app-server did not accept a request within \(timeout) s")
        func stopped() -> AppServerError? { lock.withLock { closed ? refused : failure } }
        while writeGate.wait(timeout: min(deadline, .now() + .milliseconds(50))) == .timedOut {
            if let e = stopped() { throw e }
            if DispatchTime.now() >= deadline { throw timedOut }
        }
        defer { writeGate.signal() }
        var off = 0
        do {
            try data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
                while off < raw.count {
                    if let e = stopped() { throw e }
                    guard stdinOpen else { throw refused }
                    let now = DispatchTime.now()
                    guard now < deadline else { throw timedOut }
                    let n = Darwin.write(stdinFD, raw.baseAddress! + off, raw.count - off)
                    if n >= 0 {
                        off += n
                        continue
                    }
                    if errno == EINTR { continue }
                    guard errno == EAGAIN else { throw refused }
                    let left = (deadline.uptimeNanoseconds - now.uptimeNanoseconds) / 1_000_000 + 1
                    var p = pollfd(fd: stdinFD, events: Int16(POLLOUT), revents: 0)
                    _ = poll(&p, 1, Int32(min(left, 50)))
                }
            }
        } catch {
            if off > 0 { fail(AppServerError("Codex app-server stream was cut off inside a request")) }
            throw error
        }
    }

    /// Whether JSONSerialization can encode `value` without raising: only the JSON types, finite numbers, string keys,
    /// nested at most as deep as Codex's JSON may be, so neither this check nor the encoder can exhaust the stack.
    private static func encodable(_ value: Any, depth: Int = 0) -> Bool {
        switch value {
        case is String, is NSNull, is Bool: return true
        case let n as NSNumber: return CFNumberIsFloatType(n) ? n.doubleValue.isFinite : true
        case let a as [Any]: return depth < maxCodexJSONDepth && a.allSatisfy { encodable($0, depth: depth + 1) }
        case let d as [String: Any]:
            return depth < maxCodexJSONDepth && d.values.allSatisfy { encodable($0, depth: depth + 1) }
        default: return false
        }
    }

    /// Matches replies to pending calls until stdout ends or misbehaves. A server request is answered with an error
    /// and never acted on; it and every notification are queued for `onEvent` when one is set. A line is never
    /// quoted: it may carry a credential.
    private func readLoop(_ fd: Int32) {
        defer { Darwin.close(fd) }
        var buffer: [UInt8] = []
        var scanned = 0  // bytes of `buffer` already searched for a newline
        var failed = false
        while let chunk = readChunk(fd) {
            if failed { continue }  // keep the child from blocking on a full pipe
            buffer.append(contentsOf: chunk)
            var start = 0
            while let nl = buffer[max(start, scanned)...].firstIndex(of: 0x0A) {
                if nl - start > AppServerClient.maxLine {
                    fail(AppServerError("Codex app-server sent a line longer than 4 MiB"))
                    failed = true
                    break
                }
                if !handle(buffer[start..<nl]) {
                    fail(AppServerError("Codex app-server sent malformed JSON"))
                    failed = true
                    break
                }
                start = nl + 1
                // The client is done (an overflow, a cut-off write, close): nothing more is read from the stream.
                if lock.withLock({ failure != nil }) {
                    failed = true
                    break
                }
            }
            if failed {
                buffer = []
                continue
            }
            buffer.removeFirst(start)
            scanned = buffer.count
            if buffer.count > AppServerClient.maxLine {
                fail(AppServerError("Codex app-server sent a line longer than 4 MiB"))
                failed = true
                buffer = []
            }
        }
        // A last line without a newline is still a line, as Go's bufio.Scanner reads it.
        if !failed, !buffer.isEmpty, !lock.withLock({ stopReaders }), !handle(buffer[...]) {
            fail(AppServerError("Codex app-server sent malformed JSON"))
        }
        fail(AppServerError("Codex app-server exited unexpectedly"))
    }

    private func stderrLoop(_ fd: Int32) {
        defer { Darwin.close(fd) }
        while let chunk = readChunk(fd) {
            lock.withLock {
                let room = AppServerClient.stderrCap - stderrBytes.count
                if room > 0 { stderrBytes.append(contentsOf: chunk.prefix(room)) }
            }
        }
    }

    /// The next bytes from `fd`; nil at end of file, on an error, or once the client is closed. Polls, so a close
    /// ends the reader even while a descendant holds the write end.
    private func readChunk(_ fd: Int32) -> [UInt8]? {
        var buf = [UInt8](repeating: 0, count: 65_536)
        while true {
            if lock.withLock({ stopReaders }) { return nil }
            var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            let r = poll(&p, 1, 50)
            if r < 0 && errno != EINTR { return nil }
            if r <= 0 { continue }
            let n = buf.withUnsafeMutableBytes { Darwin.read(fd, $0.baseAddress!, $0.count) }
            if n < 0 && (errno == EINTR || errno == EAGAIN) { continue }
            if n <= 0 { return nil }
            return Array(buf[0..<n])
        }
    }

    /// Handles one line (spec/credentials.md, messages); false when it is not JSON-RPC. A message is classified
    /// before its reply fields are read: a notification (no id) is ignored whatever else it holds, unless `onEvent`
    /// takes it, and a message with a method, even "", is a server request. Keys are exact.
    private func handle(_ line: ArraySlice<UInt8>) -> Bool {
        let trimmed = line.drop { Self.isSpace($0) }.reversed().drop { Self.isSpace($0) }.reversed()
        if trimmed.isEmpty { return true }
        guard isPlainJSON(trimmed),
            let value = try? JSONSerialization.jsonObject(with: Data(trimmed), options: [.fragmentsAllowed])
        else { return false }
        if value is NSNull { return true }
        guard let m = value as? [String: Any] else { return false }
        guard let id = m["id"], !(id is NSNull) else {
            queueEvent(m, line: trimmed, serverRequest: false)
            return true  // a notification, whatever else it holds
        }
        var hasMethod = false
        if let raw = m["method"], !(raw is NSNull) {
            guard raw is String else { return false }
            hasMethod = true
        }
        if hasMethod {
            refuse(id)
            queueEvent(m, line: trimmed, serverRequest: true)
            return true
        }
        let result: Result<Any, AppServerError>
        if let e = m["error"], !(e is NSNull) {
            // As in Go: a code that is not a signed 64-bit integer, or a message that is not a string, makes the line
            // malformed.
            guard let e = e as? [String: Any] else { return false }
            var code = 0
            if let raw = e["code"], !(raw is NSNull) {
                guard let n = Self.int64(raw) else { return false }
                code = Int(n)
            }
            var message = ""
            if let raw = e["message"], !(raw is NSNull) {
                guard let s = raw as? String else { return false }
                message = s
            }
            result = .failure(AppServerError("", rpcCode: code, rpcMessage: message))
        } else if let r = m["result"] {
            result = .success(r)  // NSNull for null
        } else {
            result = .failure(AppServerError(Self.missingResult))
        }
        guard let n = Self.int64(id) else { return true }  // not one of ours
        lock.withLock {
            guard let p = pending.removeValue(forKey: Int(n)) else { return }
            p.result = result
            p.done.signal()
        }
        return true
    }

    /// A JSON number written as an integer within the signed 64-bit range; nil for a fraction, an exponent, an
    /// unsigned value beyond it, a Boolean or any other type.
    private static func int64(_ v: Any) -> Int64? {
        guard let n = v as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), !CFNumberIsFloatType(n),
            NSNumber(value: n.int64Value) == n
        else { return nil }
        return n.int64Value
    }

    /// Marks the client unusable and releases every pending call.
    func fail(_ e: AppServerError) {
        lock.withLock {
            if failure == nil { failure = e }
            for p in pending.values {
                if p.result != nil { continue }
                p.result = .failure(failure!)
                p.done.signal()
            }
            pending.removeAll()
        }
        refusalWake.signal()  // the refusal writer ends
        eventWake.signal()  // the event loop delivers what is queued, then ends
    }

    /// Hands one notification or server request to ``eventLoop()``. Without a handler it does nothing, so a
    /// notification is ignored as before; a notification whose method is absent or not a string is ignored too, and
    /// so is every event once the client has failed. The params are the exact bytes of the line,
    /// as Go keeps them. Events held beyond `eventCap` or `maxEvents`, from a handler that does not keep up, fail the
    /// client.
    private func queueEvent<C: Collection>(_ m: [String: Any], line: C, serverRequest: Bool) where C.Element == UInt8 {
        guard onEvent != nil, let method = m["method"] as? String else { return }
        // The line was validated, so this reads.
        let params = (try? WireJSON.memberBytes(Data(line), "params")) ?? nil
        let event = AppServerEvent(method: method, params: params ?? Data(), isServerRequest: serverRequest)
        let size = Self.size(event)
        let over = lock.withLock { () -> Bool? in
            guard failure == nil else { return nil }  // the client is done: nothing more is delivered
            guard eventBytes + size <= Self.eventCap, eventCount + 1 <= Self.maxEvents else { return true }
            events.append(event)
            eventBytes += size
            eventCount += 1
            return false
        }
        switch over {
        case true?: fail(AppServerError("Codex app-server sent events faster than the handler took them"))
        case false?: eventWake.signal()
        case nil: break
        }
    }

    /// What an event is charged against `eventCap`.
    private static func size(_ e: AppServerEvent) -> Int { eventOverhead + e.method.utf8.count + e.params.count }

    /// Calls the handler with each queued event, one at a time, in the order the child wrote them. After the child
    /// fails or exits it still delivers what was queued; close stops it before the next event. The event being
    /// delivered stays charged against the bounds until the handler returns.
    private func eventLoop() {
        guard let onEvent else { return }
        enum Next {
            case deliver(AppServerEvent)
            case wait
            case stop
        }
        while true {
            eventWake.wait()
            drain: while true {
                let next = lock.withLock { () -> Next in
                    if closeStarted { return .stop }
                    guard eventHead < events.count else { return failure != nil ? .stop : .wait }
                    let e = events[eventHead]
                    eventHead += 1
                    if eventHead > 1024 && eventHead * 2 > events.count {  // released in O(1) amortised
                        events.removeFirst(eventHead)
                        eventHead = 0
                    }
                    return .deliver(e)
                }
                switch next {
                case .deliver(let e):
                    onEvent(e)
                    lock.withLock {
                        eventBytes -= Self.size(e)
                        eventCount -= 1
                    }
                case .wait: break drain
                case .stop: return
                }
            }
        }
    }

    /// Queues the answer to a server request (an approval, for instance): an error, written by ``refuseLoop()``. The
    /// reader never writes, so a child that blocks on its full output while we wait to write its input cannot stall
    /// it. Refusals beyond `refusalCap`, from a child that sends requests without reading the answers, fail the client.
    private func refuse(_ id: Any) {
        let message: [String: Any] = [
            "id": id, "error": ["code": AppServerError.methodNotFound, "message": "server requests are not supported"],
        ]
        guard let data = try? Self.encode(message) else {  // id is a parsed JSON value, so this cannot happen
            fail(AppServerError("Codex app-server sent a server request that could not be answered"))
            return
        }
        let over = lock.withLock { () -> Bool in
            guard refusalBytes + data.count <= Self.refusalCap else { return true }
            refusals.append(data)
            refusalBytes += data.count
            refusalsQueued += 1
            return false
        }
        if over {
            fail(AppServerError("Codex app-server sent server requests without reading the answers"))
        } else {
            refusalWake.signal()
        }
    }

    /// Writes the queued refusals in order, each with the per-call deadline, until the client fails. A refusal that
    /// misses its deadline is dropped: the child stopped reading, and the calls waiting on it time out on their own.
    private func refuseLoop() {
        while true {
            refusalWake.wait()
            let batch = lock.withLock { () -> [Data]? in
                guard failure == nil else { return nil }
                defer { refusals = [] }
                return refusals
            }
            guard let batch else { return }
            for data in batch {
                try? write(data, deadline: .now() + timeout)
                let failed = lock.withLock { () -> Bool in
                    refusalBytes -= data.count
                    refusalsDone += 1
                    return failure != nil
                }
                if failed { return }
            }
        }
    }

    /// JSON's own whitespace, and nothing else: any other space is not JSON and makes the line malformed.
    private static func isSpace(_ b: UInt8) -> Bool { b == 0x20 || b == 0x09 || b == 0x0A || b == 0x0D }

    private static func closeAll(_ fds: [Int32]) {
        for fd in fds where fd >= 0 { Darwin.close(fd) }
    }

    private static func thread(_ body: @escaping @Sendable () -> Void) {
        let t = Thread(block: body)
        t.stackSize = 1 << 20
        t.start()
    }

    private static func withCStrings<R>(
        _ strings: [String], _ body: (UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>) -> R
    ) -> R {
        var pointers: [UnsafeMutablePointer<CChar>?] = strings.map { strdup($0) }
        pointers.append(nil)
        defer { for p in pointers { free(p) } }
        return pointers.withUnsafeMutableBufferPointer { body($0.baseAddress!) }
    }
}

/// Whether `bytes` can be JSON as Go reads it: UTF-8 without a byte-order mark. JSONSerialization also accepts
/// UTF-16 and UTF-32, whose text always holds NUL bytes, and skips a leading UTF-8 byte-order mark; Go accepts
/// neither, and no JSON text holds a raw NUL.
func isPlainJSON<C: Collection>(_ bytes: C) -> Bool where C.Element == UInt8 {
    // The strict grammar also refuses duplicate keys and invalid UTF-8, which JSONSerialization and Go's
    // encoding/json each settle their own way; Codex's serde refuses both. The depth bound comes first and copies
    // nothing.
    !bytes.contains(0) && !bytes.starts(with: [0xEF, 0xBB, 0xBF]) && withinBounds(bytes, maxCodexJSONDepth)
        && WireJSON.isValid(Data(bytes))
}

/// How deeply Codex's JSON (a reply, auth.json, a JWT payload) may nest, as in Go (`codexapp.MaxJSONDepth`). Codex's
/// own documents nest a few levels; the bound is one both implementations' decoders accept (Foundation's stops at
/// 512), so a deeper document is malformed in both rather than in one.
let maxCodexJSONDepth = 64

/// Whether no array or object in `bytes` nests deeper than `max` and every number outside strings fits a Double, as
/// in Go (`codexapp.ValidJSON`). Foundation refuses 1e999 wherever it is (and reads -1e400 as -inf), and Codex's
/// serde refuses both, so such a number makes the document malformed in both implementations.
func withinBounds<C: Collection>(_ bytes: C, _ max: Int) -> Bool where C.Element == UInt8 {
    var depth = 0, inString = false, escaped = false
    var number: [UInt8] = []
    func numberFits() -> Bool {
        defer { number.removeAll(keepingCapacity: true) }
        guard !number.isEmpty, let d = Double(String(decoding: number, as: UTF8.self)) else { return true }
        return d.isFinite  // a malformed number is the strict check's to refuse
    }
    for c in bytes {
        if !inString, c == 0x2D || c == 0x2B || c == 0x2E || c == 0x65 || c == 0x45 || (c >= 0x30 && c <= 0x39) {
            if !number.isEmpty || c == 0x2D || (c >= 0x30 && c <= 0x39) {
                number.append(c)
                continue
            }
        }
        if !number.isEmpty, !numberFits() { return false }
        if inString {
            if escaped {
                escaped = false
            } else if c == 0x5C {
                escaped = true
            } else if c == 0x22 {
                inString = false
            }
        } else if c == 0x22 {
            inString = true
        } else if c == 0x5B || c == 0x7B {
            depth += 1
            if depth > max { return false }
        } else if c == 0x5D || c == 0x7D {
            depth -= 1
        }
    }
    return numberFits()
}

/// A JSON object, with null read as an empty one.
private func object(_ v: Any) -> [String: Any]? {
    if v is NSNull { return [:] }
    return v as? [String: Any]
}

/// `.some(nil)` for absent or null, `.some(s)` for a string, nil for any other type.
private func optionalString(_ v: Any?) -> String?? {
    guard let v, !(v is NSNull) else { return .some(nil) }
    guard let s = v as? String else { return nil }
    return .some(s)
}

private func bool(_ v: Any) -> Bool? {
    guard let n = v as? NSNumber, CFGetTypeID(n) == CFBooleanGetTypeID() else { return nil }
    return n.boolValue
}
#endif
