#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// A failure of the app-server child: it did not start, crashed, timed out, misbehaved, or answered a call with a
/// JSON-RPC error. `message` never quotes the child's output; `rpcMessage` is scrubbed and kept out of
/// `description`, because a misbehaving child may put a credential there.
public struct AppServerError: Error, Sendable, Equatable, CustomStringConvertible {
    /// The JSON-RPC code of an error reply; nil for any other failure.
    public let rpcCode: Int?
    public let rpcMessage: String
    public let message: String

    public init(_ message: String, rpcCode: Int? = nil, rpcMessage: String = "") {
        self.message = message
        self.rpcCode = rpcCode
        self.rpcMessage = Secret.scrub(rpcMessage)
    }

    /// The JSON-RPC code for an unknown method.
    public static let methodNotFound = -32601

    public var description: String {
        rpcCode.map { "\(message): JSON-RPC error \($0)" } ?? message
    }
}

/// The getAuthStatus result. The token is registered with the scrubber and redacted by every formatting path,
/// reflection (`dump`, `Mirror`) included.
public struct AuthStatus: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible,
    CustomReflectable
{
    /// `authMethod`; "" when Codex reports null (not signed in).
    public let method: String
    /// `requiresOpenaiAuth`; false means a provider needing no OpenAI sign-in is in force.
    public let requiresOpenAIAuth: Bool
    /// The credential Codex sent, or "".
    public let token: String

    public init(method: String, requiresOpenAIAuth: Bool, token: String) {
        Secret.register(token)
        self.method = method
        self.requiresOpenAIAuth = requiresOpenAIAuth
        self.token = token
    }

    public var description: String { "AuthStatus{method: \(Secret.scrub(method))}" }
    public var debugDescription: String { description }
    public var customMirror: Mirror {
        Mirror(
            self,
            children: [
                "method": Secret.scrub(method), "requiresOpenAIAuth": requiresOpenAIAuth, "token": Secret.redacted,
            ], displayStyle: .struct)
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

        public init(
            bin: String, codexHome: String? = nil, environment: [String: String] = ProcessInfo.processInfo.environment,
            timeout: TimeInterval, clientName: String = "sayso", guardStart: (@Sendable (String) throws -> Void)? = nil,
            log: (@Sendable (String) -> Void)? = nil
        ) {
            self.bin = bin
            self.codexHome = codexHome
            self.environment = environment
            self.timeout = timeout
            self.clientName = clientName
            self.guardStart = guardStart
            self.log = log
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

    /// The child's process ID.
    public var pid: pid_t { core.pid }
    /// The initialize `userAgent`, scrubbed; "" when absent.
    public private(set) var userAgent = ""

    /// The child and its readers. The reader threads hold the core, never the client, so releasing an unclosed
    /// client runs its deinit, which ends the child.
    private let core: AppServerCore

    private init(core: AppServerCore) { self.core = core }

    /// Spawns the child in its own process group and completes the initialize handshake.
    public static func start(_ cfg: Configuration) throws -> AppServerClient {
        guard cfg.bin.hasPrefix("/"), !cfg.bin.contains("\0") else {
            throw AppServerError("the Codex executable path is not absolute")
        }
        guard cfg.timeout > 0, cfg.timeout.isFinite else { throw AppServerError("a per-call timeout is required") }
        if let guardStart = cfg.guardStart {
            do { try guardStart(cfg.bin) } catch {
                throw AppServerError("starting Codex was refused: \(Secret.scrub(String(describing: error)))")
            }
        }
        let env = CodexExecutable.childEnvironment(cfg.environment, bin: cfg.bin, codexHome: cfg.codexHome)
        let c = AppServerClient(core: try AppServerCore(bin: cfg.bin, env: env, timeout: cfg.timeout, log: cfg.log))
        do {
            let hello = try c.core.call(
                "initialize",
                params: ["clientInfo": ["name": cfg.clientName, "version": "0"], "capabilities": [String: Any]()])
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
        return AuthStatus(method: method ?? "", requiresOpenAIAuth: requires, token: token ?? "")
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
    private let writeLock = NSLock()
    private var nextID = 0
    private var pending: [Int: Pending] = [:]
    private var failure: AppServerError?
    private var stopReaders = false
    private var closed = false
    private var stdinOpen = true  // guarded by writeLock
    private var stderrBytes: [UInt8] = []
    private let exited = DispatchSemaphore(value: 0)

    private final class Pending {
        let done = DispatchSemaphore(value: 0)
        var result: Result<Any, AppServerError>?
    }

    init(bin: String, env: [String: String], timeout: TimeInterval, log: (@Sendable (String) -> Void)?) throws {
        self.timeout = timeout
        self.log = log
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
        Self.thread { [self] in stderrLoop(err) }
        Self.thread { [self] in
            var status: Int32 = 0
            while waitpid(childPID, &status, 0) == -1 && errno == EINTR {}
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

    func close() {
        let first = lock.withLock { () -> Bool in
            defer { closed = true }
            return !closed
        }
        guard first else { return }
        // A writer waiting on a full pipe sees `closed` within one poll interval and lets go of the lock.
        writeLock.withLock {
            if stdinOpen {
                Darwin.close(stdinFD)
                stdinOpen = false
            }
        }
        _ = kill(-pid, SIGTERM)
        if !waitGroup(until: .now() + AppServerClient.killGrace) {
            _ = kill(-pid, SIGKILL)
            if !reaped(by: .now() + AppServerClient.reapGrace) {
                log?("codex app-server was not reaped within the grace; abandoning it (pid \(pid))")
            }
        }
        let n = lock.withLock { () -> Int in
            stopReaders = true
            return stderrBytes.count
        }
        fail(AppServerError("Codex app-server was closed"))
        if n > 0 { log?("codex app-server wrote to stderr (\(n) bytes)") }
    }

    var hasExited: Bool { reaped(by: .now()) }

    /// Whether the child was reaped by `deadline`; the semaphore keeps its signal for the next look.
    private func reaped(by deadline: DispatchTime) -> Bool {
        guard exited.wait(timeout: deadline) == .success else { return false }
        exited.signal()
        return true
    }

    /// Whether the child was reaped and no other member of its process group remains, by `deadline`. A group
    /// outlives its leader while a member lives, so the group ID still names our group while kill(-pid, 0) succeeds.
    private func waitGroup(until deadline: DispatchTime) -> Bool {
        while true {
            if reaped(by: .now()) && !groupAlive() { return true }
            let now = DispatchTime.now()
            if now >= deadline { return false }
            _ = reaped(by: min(deadline, now + .milliseconds(20)))
        }
    }

    private func groupAlive() -> Bool { kill(-pid, 0) == 0 || errno == EPERM }

    // MARK: - Wire

    /// Writes one message, giving up at `deadline`, so a child that stops reading cannot hold a call, or the
    /// reader refusing a server request, past its timeout; ``close()`` ends the wait too.
    private func write(_ message: [String: Any], deadline: DispatchTime) throws {
        guard var data = try? JSONSerialization.data(withJSONObject: message, options: [.withoutEscapingSlashes]) else {
            throw AppServerError("a request could not be encoded")
        }
        data.append(0x0A)
        let refused = AppServerError("Codex app-server is not accepting requests")
        try writeLock.withLock {
            try data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
                var off = 0
                while off < raw.count {
                    guard stdinOpen, !lock.withLock({ closed }) else { throw refused }
                    let n = Darwin.write(stdinFD, raw.baseAddress! + off, raw.count - off)
                    if n >= 0 {
                        off += n
                        continue
                    }
                    if errno == EINTR { continue }
                    guard errno == EAGAIN else { throw refused }
                    let now = DispatchTime.now()
                    guard now < deadline else {
                        throw AppServerError("Codex app-server did not accept a request within \(timeout) s")
                    }
                    let left = (deadline.uptimeNanoseconds - now.uptimeNanoseconds) / 1_000_000 + 1
                    var p = pollfd(fd: stdinFD, events: Int16(POLLOUT), revents: 0)
                    _ = poll(&p, 1, Int32(min(left, 50)))
                }
            }
        }
    }

    /// Matches replies to pending calls until stdout ends or misbehaves. A server request is answered with an error
    /// and never acted on. A line is never quoted: it may carry a credential.
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

    /// Handles one line; false when it is not JSON-RPC.
    private func handle(_ line: ArraySlice<UInt8>) -> Bool {
        let trimmed = line.drop { Self.isSpace($0) }.reversed().drop { Self.isSpace($0) }.reversed()
        if trimmed.isEmpty { return true }
        guard isPlainJSON(trimmed),
            let value = try? JSONSerialization.jsonObject(with: Data(trimmed), options: [.fragmentsAllowed])
        else { return false }
        if value is NSNull { return true }
        guard let m = value as? [String: Any] else { return false }
        var method = ""
        if let raw = m["method"], !(raw is NSNull) {
            guard let s = raw as? String else { return false }
            method = s
        }
        guard let id = m["id"], !(id is NSNull) else { return true }  // a notification
        if !method.isEmpty {
            try? write(
                [
                    "id": id,
                    "error": ["code": AppServerError.methodNotFound, "message": "server requests are not supported"],
                ], deadline: .now() + timeout)
            return true
        }
        let result: Result<Any, AppServerError>
        if let e = m["error"], !(e is NSNull) {
            // As in Go: a code that is not an integer, or a message that is not a string, makes the line malformed.
            guard let e = e as? [String: Any] else { return false }
            var code = 0
            if let raw = e["code"], !(raw is NSNull) {
                guard let n = raw as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), !CFNumberIsFloatType(n)
                else { return false }
                code = n.intValue
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
        guard let n = id as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), !CFNumberIsFloatType(n) else {
            return true  // not one of ours
        }
        lock.withLock {
            guard let p = pending.removeValue(forKey: n.intValue) else { return }
            p.result = result
            p.done.signal()
        }
        return true
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
    }

    private static func isSpace(_ b: UInt8) -> Bool {
        b == 0x20 || b == 0x09 || b == 0x0A || b == 0x0D || b == 0x0B || b == 0x0C
    }

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
    // encoding/json each settle their own way; Codex's serde refuses both.
    !bytes.contains(0) && !bytes.starts(with: [0xEF, 0xBB, 0xBF]) && WireJSON.isValid(Data(bytes))
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
