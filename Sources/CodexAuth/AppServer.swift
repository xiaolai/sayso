#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// A failure of the app-server child: it did not start, crashed, timed out, misbehaved, or answered a call with a
/// JSON-RPC error. `message` never quotes the child's output; `rpcMessage` is scrubbed and kept out of
/// `description`, because a misbehaving child may put a credential there.
public struct AppServerError: Error, Sendable, Equatable, CustomStringConvertible {
    /// The JSON-RPC error code of an error reply; nil for any other failure.
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

/// The getAuthStatus result. The token is registered with the scrubber and redacted by every formatting path.
public struct AuthStatus: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible {
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
}

/// One `codex app-server --listen stdio://` child, alive until ``close()``, speaking newline-delimited JSON-RPC
/// (spec/credentials.md, the app-server source). Its calls block the calling thread up to the per-call timeout, so
/// call them off the main thread and off Swift's cooperative pool.
public final class AppServerClient: @unchecked Sendable {
    /// How to start the child.
    public struct Configuration: Sendable {
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
    }

    /// The child's arguments. The provider override makes Codex answer for its OpenAI sign-in even when the user's
    /// configured provider needs none.
    public static let arguments = ["-c", #"model_provider="openai""#, "app-server", "--listen", "stdio://"]

    static let killGrace: TimeInterval = 2
    static let reapGrace: TimeInterval = 1
    static let stderrCap = 64 << 10
    static let maxLine = 4 << 20

    /// The child's process ID.
    public let pid: pid_t
    /// The initialize `userAgent`, scrubbed; "" when absent.
    public private(set) var userAgent = ""

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
    private var stdinOpen = true
    private var stderrBytes: [UInt8] = []
    private let exited = DispatchSemaphore(value: 0)

    private final class Pending {
        let done = DispatchSemaphore(value: 0)
        var result: Result<Any, AppServerError>?
    }

    /// Spawns the child in its own process group and completes the initialize handshake.
    public static func start(_ cfg: Configuration) throws -> AppServerClient {
        guard cfg.bin.hasPrefix("/") else { throw AppServerError("the Codex executable path is not absolute") }
        guard cfg.timeout > 0 else { throw AppServerError("a per-call timeout is required") }
        if let guardStart = cfg.guardStart {
            do { try guardStart(cfg.bin) } catch {
                throw AppServerError("starting Codex was refused: \(Secret.scrub(String(describing: error)))")
            }
        }
        let env = CodexExecutable.childEnvironment(cfg.environment, bin: cfg.bin, codexHome: cfg.codexHome)
        let c = try AppServerClient(bin: cfg.bin, env: env, timeout: cfg.timeout, log: cfg.log)
        do {
            let hello = try c.call(
                "initialize",
                params: ["clientInfo": ["name": cfg.clientName, "version": "0"], "capabilities": [String: Any]()])
            guard let object = hello as? [String: Any], let agent = optionalString(object["userAgent"]) else {
                throw AppServerError("Codex app-server sent an unexpected initialize result")
            }
            c.userAgent = Secret.scrub(agent ?? "")
            try c.write(["method": "initialized"])
        } catch {
            c.close()
            throw error
        }
        return c
    }

    private init(bin: String, env: [String: String], timeout: TimeInterval, log: (@Sendable (String) -> Void)?) throws {
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

        let argv = [bin] + Self.arguments
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

    deinit { close() }

    /// The first 64 KiB the child wrote to stderr, scrubbed. It is for diagnostics a caller chooses to show; the
    /// package never logs it, because the scrubber cannot recognise every credential format.
    public var stderr: String {
        lock.withLock { Secret.scrub(String(decoding: stderrBytes, as: UTF8.self)) }
    }

    /// Sends one request and returns its `result`, which is NSNull when absent or null. Fails after the per-call
    /// timeout, on a crash or on malformed output; a JSON-RPC error reply carries its code.
    public func call(_ method: String, params: [String: Any] = [:]) throws -> Any {
        let p = Pending()
        let id: Int = try lock.withLock {
            if let failure { throw failure }
            nextID += 1
            pending[nextID] = p
            return nextID
        }
        defer { _ = lock.withLock { pending.removeValue(forKey: id) } }
        try write(["id": id, "method": method, "params": params])
        if p.done.wait(timeout: .now() + timeout) == .timedOut {
            throw AppServerError("Codex app-server did not answer \(method) within \(timeout) s")
        }
        switch lock.withLock({ p.result }) {
        case .success(let result): return result
        case .failure(let e) where e.rpcCode != nil:
            throw AppServerError("Codex app-server rejected \(method)", rpcCode: e.rpcCode, rpcMessage: e.rpcMessage)
        case .failure(let e): throw e
        case nil: throw AppServerError("Codex app-server exited unexpectedly")
        }
    }

    /// getAuthStatus. Any token in the reply is registered with the scrubber before it is returned.
    public func getAuthStatus(includeToken: Bool, refreshToken: Bool) throws -> AuthStatus {
        let result = try call("getAuthStatus", params: ["includeToken": includeToken, "refreshToken": refreshToken])
        let bad = AppServerError("Codex app-server sent an unexpected getAuthStatus result")
        guard let o = object(result) else { throw bad }
        guard let method = optionalString(o["authMethod"]), let token = optionalString(o["authToken"]) else { throw bad }
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

    /// account/read's `account.planType`, "" when absent. Only this field is read. account/read may reach the network.
    public func planType() throws -> String {
        let result = try call("account/read")
        guard let o = object(result) else { throw AppServerError("Codex app-server sent an unexpected account/read result") }
        guard let account = o["account"] as? [String: Any] else { return "" }
        return Secret.scrub(account["planType"] as? String ?? "")
    }

    /// Ends the child: stdin closed, SIGTERM to its process group, SIGKILL to the group after 2 s. Returns within
    /// grace + 1 s even when a descendant outside the group still holds the pipes: the readers are abandoned and
    /// close their ends. Safe to call more than once.
    public func close() {
        let first = lock.withLock { () -> Bool in
            defer { closed = true }
            return !closed
        }
        guard first else { return }
        writeLock.withLock {
            if stdinOpen {
                Darwin.close(stdinFD)
                stdinOpen = false
            }
        }
        _ = kill(-pid, SIGTERM)
        if exited.wait(timeout: .now() + Self.killGrace) == .timedOut {
            _ = kill(-pid, SIGKILL)
            if exited.wait(timeout: .now() + Self.reapGrace) == .timedOut {
                log?("codex app-server was not reaped within the grace; abandoning it (pid \(pid))")
            } else {
                exited.signal()
            }
        } else {
            exited.signal()
        }
        let n = lock.withLock { () -> Int in
            stopReaders = true
            return stderrBytes.count
        }
        fail(AppServerError("Codex app-server was closed"))
        if n > 0 { log?("codex app-server wrote to stderr (\(n) bytes)") }
    }

    /// Whether the child has been reaped.
    var hasExited: Bool {
        guard exited.wait(timeout: .now()) == .success else { return false }
        exited.signal()
        return true
    }

    // MARK: - Wire

    private func write(_ message: [String: Any]) throws {
        guard var data = try? JSONSerialization.data(withJSONObject: message, options: [.withoutEscapingSlashes]) else {
            throw AppServerError("a request could not be encoded")
        }
        data.append(0x0A)
        try writeLock.withLock {
            guard stdinOpen else { throw AppServerError("Codex app-server is not accepting requests") }
            let ok = data.withUnsafeBytes { raw -> Bool in
                var off = 0
                while off < raw.count {
                    let n = Darwin.write(stdinFD, raw.baseAddress! + off, raw.count - off)
                    if n < 0 {
                        if errno == EINTR { continue }
                        return false
                    }
                    off += n
                }
                return true
            }
            if !ok { throw AppServerError("Codex app-server is not accepting requests") }
        }
    }

    /// Matches replies to pending calls until stdout ends or misbehaves. A server request is answered with an error
    /// and never acted on. A line is never quoted: it may carry a credential.
    private func readLoop(_ fd: Int32) {
        defer { Darwin.close(fd) }
        var buffer: [UInt8] = []
        var failed = false
        while let chunk = readChunk(fd) {
            if failed { continue }  // keep the child from blocking on a full pipe
            buffer.append(contentsOf: chunk)
            var start = 0
            while let nl = buffer[start...].firstIndex(of: 0x0A) {
                if nl - start > Self.maxLine {
                    fail(AppServerError("Codex app-server sent a line longer than 4 MiB"))
                    failed = true
                    break
                }
                if !handle(Array(buffer[start..<nl])) {
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
            if buffer.count > Self.maxLine {
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
                let room = Self.stderrCap - stderrBytes.count
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
    private func handle(_ line: [UInt8]) -> Bool {
        let trimmed = line.drop { Self.isSpace($0) }.reversed().drop { Self.isSpace($0) }.reversed()
        if trimmed.isEmpty { return true }
        guard let value = try? JSONSerialization.jsonObject(with: Data(trimmed), options: [.fragmentsAllowed]) else {
            return false
        }
        if value is NSNull { return true }
        guard let m = value as? [String: Any] else { return false }
        var method = ""
        if let raw = m["method"], !(raw is NSNull) {
            guard let s = raw as? String else { return false }
            method = s
        }
        guard let id = m["id"], !(id is NSNull) else { return true }  // a notification
        if !method.isEmpty {
            try? write([
                "id": id, "error": ["code": AppServerError.methodNotFound, "message": "server requests are not supported"],
            ])
            return true
        }
        var result: Result<Any, AppServerError> = .success(m["result"] ?? NSNull())
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
    private func fail(_ e: AppServerError) {
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

    private static func isSpace(_ b: UInt8) -> Bool { b == 0x20 || b == 0x09 || b == 0x0A || b == 0x0D || b == 0x0B || b == 0x0C }

    private static func closeAll(_ fds: [Int32]) {
        for fd in fds where fd >= 0 { Darwin.close(fd) }
    }

    private static func thread(_ body: @escaping @Sendable () -> Void) {
        let t = Thread(block: body)
        t.stackSize = 1 << 20
        t.start()
    }

    private static func withCStrings<R>(_ strings: [String], _ body: (UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>) -> R) -> R {
        var pointers: [UnsafeMutablePointer<CChar>?] = strings.map { strdup($0) }
        pointers.append(nil)
        defer { for p in pointers { free(p) } }
        return pointers.withUnsafeMutableBufferPointer { body($0.baseAddress!) }
    }
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
