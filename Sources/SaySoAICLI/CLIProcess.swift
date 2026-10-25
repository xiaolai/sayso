#if os(macOS)
import Darwin
import Foundation
import SaySoAI

/// **One child process, in a process group of its own, with bounded pipes and a deadline.**
///
/// The group is what makes cancellation real: `claude` and `codex` start children of their own, and a signal to the
/// launcher alone leaves them running. Arguments are an array — no shell ever parses them — the environment is exactly
/// what the caller built, the working directory is the app's own, and both output streams are capped: a chatty or
/// hostile child cannot grow the app's memory, and stderr is kept only for classification, never logged.
///
/// **The run owns its process group.** It ends when the launcher exits; a descendant it left behind is killed rather
/// than left holding the pipes. Cancellation and the deadline end the group too, and no cleanup step runs on the
/// cooperative pool or observes cancellation, so a cancelled ask still cleans up completely.
struct CLIProcess {
    /// How much of each stream is kept. Lines past the cap are read and dropped, so the child never blocks on a full
    /// pipe.
    static let stdoutCap = 1 << 20
    static let stderrCap = 1 << 16
    /// The grace between `SIGTERM` and `SIGKILL`, and the longest wait for the pipe threads after the child is reaped.
    static let killGrace: Duration = .seconds(2)
    static let readerJoin: Duration = .seconds(1)
    /// How often the child is polled while it runs.
    static let pollInterval: Duration = .milliseconds(10)
    /// The longest line kept for `onLine`; a longer one is dropped through its newline, and parsing resumes after it.
    static let lineCap = 256 << 10

    struct Run: Sendable, Equatable {
        let status: Int32
        let stdout: String
        let stderr: String
    }

    enum Failure: Error, Equatable, Sendable {
        /// The executable is not there, not a regular file, or not executable by this process.
        case notInstalled
        case spawn(Int32)
        case timeout
        case cancelled
        /// A pipe thread could not be read to its end, or would not stop: the output may be incomplete, and an
        /// incomplete stream is never presented as the child's answer.
        case readerFailed
    }

    private struct Child {
        let pid: pid_t
        let stdoutFD: Int32
        let stderrFD: Int32
        let stdinFD: Int32
    }

    /// Runs `executable` to exit, feeding `input` on stdin, within `deadline`.
    ///
    /// `onLine` is called for each complete line of **either** stream as it arrives — a sign-in instruction may be
    /// printed on stderr, and the reader needs it while the child still runs. It is called from a pipe thread and must
    /// be cheap and thread-safe.
    static func run(
        executable: URL, arguments: [String], environment: [String: String], workingDirectory: URL,
        input: String? = nil, deadline: Duration, onLine: (@Sendable (String) -> Void)? = nil
    ) async throws(Failure) -> Run {
        // **Cancellation wins over starting anything**: a task cancelled before the spawn never creates a child.
        guard !Task.isCancelled else { throw .cancelled }
        guard isExecutable(executable) else { throw .notInstalled }
        let child = try spawn(executable: executable, arguments: arguments, environment: environment,
                              workingDirectory: workingDirectory)
        // **A child may exit before reading its prompt.** Without this, the writer's `write` raises SIGPIPE and
        // terminates the host app; with it, the write returns EPIPE and the writer stops.
        guard fcntl(child.stdinFD, F_SETNOSIGPIPE, 1) != -1 else {
            let code = errno
            close(child.stdinFD)
            close(child.stdoutFD)
            close(child.stderrFD)
            await reap(child.pid)
            throw .spawn(code)
        }

        // **One dedicated thread reads both pipes without blocking the shared pool** (2026-10-23): the readers used to
        // be blocking work on `DispatchQueue.global()`, and a starved pool made the reader join time out and leave two
        // descriptors open per timeout — under 32 concurrent runs, until `posix_spawn` itself failed with EMFILE
        // (`spawn(24)`), which turned the CLI provider tests red under a loaded suite. The thread polls non-blocking, so
        // it always ends by itself or on `stop`, and it owns its descriptors to the end.
        let stdoutText = Locked("")
        let stderrText = Locked("")
        let stopping = Locked(false)
        let readerFailed = Locked(false)
        let reader = Thread {
            Self.pump(stdout: child.stdoutFD, stderr: child.stderrFD, onLine: onLine, stop: stopping,
                      failed: readerFailed, out: stdoutText, err: stderrText)
        }
        reader.start()

        // **The prompt goes out on a thread of its own, non-blocking**: a child that stops reading, or a descendant
        // holding its stdin, must not park a pool thread or outlive the ask.
        let writeError = Locked(Int32(0))
        var writer: Thread?
        if let input {
            let thread = Thread { Self.feed(child.stdinFD, Array(input.utf8), stop: stopping, error: writeError) }
            thread.start()
            writer = thread
        } else {
            close(child.stdinFD)
        }

        let clock = ContinuousClock()
        let status: Int32
        do {
            status = try await waitFor(pid: child.pid, until: clock.now + deadline)
        } catch {
            if case .spawn(ECHILD) = error {
                // The child was reaped by somebody else: its pid may already name another process, so it is never
                // signalled — only this run's own pipes are ended.
                _ = await join(reader, stop: stopping)
                if let writer { _ = await join(writer, stop: stopping) }
                throw error
            }
            await reap(child.pid)
            _ = await join(reader, stop: stopping)
            if let writer { _ = await join(writer, stop: stopping) }
            throw error
        }
        // **The launcher's exit ends the run's process group**: a descendant it left behind — a helper still holding
        // the pipes — is killed rather than left to keep them open. An empty group answers ESRCH.
        kill(-child.pid, SIGKILL)
        let readerJoined = await join(reader, stop: stopping)
        var writerJoined = true
        if let writer { writerJoined = await join(writer, stop: stopping) }
        let writeFailure = writeError.withLock { $0 }
        // A prompt the pipe refused for any reason other than the child's own early close is a failure of this run;
        // when the child itself failed, its status is the more truthful answer and classification uses it.
        if status == 0, writeFailure != 0 { throw .spawn(writeFailure) }
        // **Both pipe threads are confirmed**, not assumed: a writer still holding stdin's descriptor would leak it and
        // could still be mid-write while the answer is returned.
        guard readerJoined, writerJoined, !readerFailed.withLock({ $0 }) else { throw .readerFailed }
        return Run(status: status, stdout: stdoutText.withLock { $0 }, stderr: stderrText.withLock { $0 })
    }

    // MARK: - Spawning

    /// Everything up to a running child, with every step checked: three pipes, close-on-exec on each, the file actions
    /// and attributes, and the argument and environment copies. A failure unwinds every descriptor and every allocation
    /// it made.
    private static func spawn(executable: URL, arguments: [String], environment: [String: String],
                              workingDirectory: URL) throws(Failure) -> Child {
        var outPipe = [Int32](repeating: -1, count: 2)
        var errPipe = [Int32](repeating: -1, count: 2)
        var inPipe = [Int32](repeating: -1, count: 2)
        guard pipe(&outPipe) == 0 else { throw .spawn(errno) }
        guard pipe(&errPipe) == 0 else {
            closeAll(outPipe)
            throw .spawn(errno)
        }
        guard pipe(&inPipe) == 0 else {
            closeAll(outPipe, errPipe)
            throw .spawn(errno)
        }
        var handedOver = false
        defer { if !handedOver { closeAll(outPipe, errPipe, inPipe) } }
        // Every end the parent created is close-on-exec: the child gets only the three descriptors `dup2` gives it, and
        // in particular it does not inherit the write end of its own stdin — with that open, EOF never arrives and a
        // child that reads stdin to the end hangs until the deadline.
        for fd in outPipe + errPipe + inPipe {
            guard fcntl(fd, F_SETFD, FD_CLOEXEC) != -1 else { throw .spawn(errno) }
        }

        var actions: posix_spawn_file_actions_t?
        let initialized = posix_spawn_file_actions_init(&actions)
        guard initialized == 0 else { throw .spawn(initialized) }
        defer { posix_spawn_file_actions_destroy(&actions) }
        var code = posix_spawn_file_actions_adddup2(&actions, inPipe[0], STDIN_FILENO)
        guard code == 0 else { throw .spawn(code) }
        code = posix_spawn_file_actions_adddup2(&actions, outPipe[1], STDOUT_FILENO)
        guard code == 0 else { throw .spawn(code) }
        code = posix_spawn_file_actions_adddup2(&actions, errPipe[1], STDERR_FILENO)
        guard code == 0 else { throw .spawn(code) }
        let directory = workingDirectory.path
        directory.withCString { path in
            // The standard spelling starts at macOS 26; `_np` reaches back to 10.15 and is deprecated at 26. This
            // package builds for macOS 14, so the older one carries every system before 26. `#available` is a runtime
            // check and cannot hide a function the SDK lacks: the standard one ships in the macOS 26 SDK, whose
            // compiler is Swift 6.2, and an earlier SDK builds with `_np` alone.
            #if compiler(>=6.2)
            if #available(macOS 26, *) {
                code = posix_spawn_file_actions_addchdir(&actions, path)
            } else {
                code = posix_spawn_file_actions_addchdir_np(&actions, path)
            }
            #else
            code = posix_spawn_file_actions_addchdir_np(&actions, path)
            #endif
        }
        guard code == 0 else { throw .spawn(code) }

        var attributes: posix_spawnattr_t?
        let attributed = posix_spawnattr_init(&attributes)
        guard attributed == 0 else { throw .spawn(attributed) }
        defer { posix_spawnattr_destroy(&attributes) }
        // **Close by default**: the child inherits only the three descriptors the file actions give it. Close-on-exec on
        // our own ends cannot cover a concurrent run's pipes in the moment between `pipe` and `fcntl`.
        code = posix_spawnattr_setflags(&attributes, Int16(POSIX_SPAWN_SETPGROUP | POSIX_SPAWN_CLOEXEC_DEFAULT))
        guard code == 0 else { throw .spawn(code) }
        // A process group of its own, named by the child's pid: signals reach everything it starts.
        code = posix_spawnattr_setpgroup(&attributes, 0)
        guard code == 0 else { throw .spawn(code) }

        let argv = try CStringArray([executable.path] + arguments)
        let envp = try CStringArray(environment.map { "\($0.key)=\($0.value)" })
        guard !Task.isCancelled else { throw .cancelled }
        var pid: pid_t = 0
        let failure = executable.path.withCString { path in
            posix_spawn(&pid, path, &actions, &attributes, argv.pointers, envp.pointers)
        }
        guard failure == 0 else { throw .spawn(failure) }
        // The parent keeps only the three ends it uses: stdout and stderr for reading, stdin for writing.
        close(inPipe[0])
        close(outPipe[1])
        close(errPipe[1])
        inPipe[0] = -1
        outPipe[1] = -1
        errPipe[1] = -1
        handedOver = true
        return Child(pid: pid, stdoutFD: outPipe[0], stderrFD: errPipe[0], stdinFD: inPipe[1])
    }

    /// A NULL-terminated C string array for `posix_spawn`. Every string is copied and every copy is freed, and an
    /// allocation that fails is a typed failure — never an argument list that silently ends early.
    private final class CStringArray {
        let pointers: [UnsafeMutablePointer<CChar>?]

        init(_ strings: [String]) throws(Failure) {
            var pointers: [UnsafeMutablePointer<CChar>?] = []
            pointers.reserveCapacity(strings.count + 1)
            for string in strings {
                guard let copy = strdup(string) else {
                    for pointer in pointers { free(pointer) }
                    throw .spawn(ENOMEM)
                }
                pointers.append(copy)
            }
            pointers.append(nil)
            self.pointers = pointers
        }

        deinit {
            for pointer in pointers where pointer != nil { free(pointer) }
        }
    }

    // MARK: - Waiting

    private static func waitFor(pid: pid_t, until deadline: ContinuousClock.Instant) async throws(Failure) -> Int32 {
        let clock = ContinuousClock()
        while true {
            var raw: Int32 = 0
            let answer = waitpid(pid, &raw, WNOHANG)
            if answer == pid { return exitCode(raw) }
            if answer < 0 {
                // EINTR is a signal, not a verdict. ECHILD says this process is not ours any more — somebody else
                // reaped it, and its pid may already name another process — so it is reported, never signalled.
                if errno == EINTR { continue }
                throw .spawn(errno)
            }
            if clock.now >= deadline { throw .timeout }
            do { try await Task.sleep(for: pollInterval) } catch { throw .cancelled }
        }
    }

    /// **The queue cleanup waits run on.** A POSIX wait parks a thread, so it must not be a cooperative-pool thread
    /// (a parked pool thread stalls unrelated work); this queue's threads are the run's own. Concurrent, because two
    /// runs may clean up at once.
    private static let cleanup = DispatchQueue(label: "sayso.cli.process.cleanup", attributes: .concurrent)

    /// Runs blocking cleanup work on `cleanup`, and beyond cancellation's reach: a cancelled ask still cleans up
    /// completely, and no step here observes the task's cancellation (a `Task.sleep` would throw at once and spin).
    private static func blocked<T: Sendable>(_ body: @escaping @Sendable () -> T) async -> T {
        await withCheckedContinuation { continuation in
            cleanup.async { continuation.resume(returning: body()) }
        }
    }

    /// Ends the child's process group and reaps it, off the cooperative pool.
    private static func reap(_ pid: pid_t) async {
        await blocked { reapBlocking(pid) }
    }

    /// `SIGTERM`, a bounded grace, then `SIGKILL`; and every wait after it bounded, because a child that cannot be
    /// reaped even after `SIGKILL` is not something this thread can fix, and an unbounded wait would hold cleanup.
    private static func reapBlocking(_ pid: pid_t) {
        kill(-pid, SIGTERM)
        kill(pid, SIGTERM)
        let clock = ContinuousClock()
        let grace = clock.now + killGrace
        while clock.now < grace {
            var raw: Int32 = 0
            let answer = waitpid(pid, &raw, WNOHANG)
            if answer == pid {
                // **The launcher is gone; its group may not be.** A descendant that ignored SIGTERM is killed now; if
                // the group is empty this is a harmless ESRCH. The pid itself is never signalled after its reap — it
                // may already belong to another process.
                kill(-pid, SIGKILL)
                return
            }
            if answer < 0 {
                // **ECHILD (or any other error) means this process is not ours any more**: somebody else reaped it, its
                // pid may name another process now, and nothing here may signal it.
                if errno != EINTR { return }
                usleep(5_000)
                continue
            }
            usleep(5_000)
        }
        kill(-pid, SIGKILL)
        kill(pid, SIGKILL)
        let giveUp = clock.now + killGrace
        while clock.now < giveUp {
            var raw: Int32 = 0
            let answer = waitpid(pid, &raw, WNOHANG)
            if answer == pid { return }
            if answer < 0, errno != EINTR { return }
            usleep(5_000)
        }
    }

    /// `Thread` is not `Sendable`, but `isFinished` is thread-safe — it is exactly what a join from another thread
    /// reads. Wrapped so the wait can run in a detached task.
    private struct JoinableThread: @unchecked Sendable {
        let thread: Thread
    }

    /// Waits for a pipe thread to end: at EOF by itself, within `readerJoin`; then asks it to stop and gives it a
    /// bounded moment more. False means it is still running — an incomplete stream, reported, never silently accepted.
    ///
    /// The wait runs on `cleanup`: `Task.sleep` throws at once in a cancelled task, which would turn this into a busy
    /// spin, and a cooperative-pool thread must not be parked on a poll.
    private static func join(_ thread: Thread, stop: Locked<Bool>) async -> Bool {
        let joinable = JoinableThread(thread: thread)
        return await blocked { () -> Bool in
            let clock = ContinuousClock()
            let until = clock.now + readerJoin
            while clock.now < until {
                if joinable.thread.isFinished { return true }
                usleep(5_000)
            }
            stop.withLock { $0 = true }
            let last = clock.now + .milliseconds(500)
            while clock.now < last {
                if joinable.thread.isFinished { return true }
                usleep(5_000)
            }
            return joinable.thread.isFinished
        }
    }

    static func exitCode(_ raw: Int32) -> Int32 {
        let term = raw & 0x7f
        if term == 0 { return (raw >> 8) & 0xff }
        return 128 + term
    }

    // MARK: - Writing

    /// The prompt, non-blocking, on its own thread. A child that closes its stdin early ends the write without an
    /// error — its exit status and output decide the ask. Every other failure is recorded for the run to report.
    private static func feed(_ fd: Int32, _ bytes: [UInt8], stop: Locked<Bool>, error: Locked<Int32>) {
        guard fcntl(fd, F_SETFL, O_NONBLOCK) != -1 else {
            error.withLock { $0 = errno }
            close(fd)
            return
        }
        var offset = 0
        while offset < bytes.count, !stop.withLock({ $0 }) {
            var descriptor = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
            let ready = poll(&descriptor, 1, 100)
            if ready < 0 {
                if errno == EINTR { continue }
                error.withLock { $0 = errno }
                break
            }
            if ready == 0 { continue }
            if descriptor.revents & Int16(POLLNVAL) != 0 {
                // **POLLNVAL is this code's own bug** — stdin was closed under the writer — and is recorded; POLLHUP or
                // POLLERR is the child's early close, which is not a failure.
                error.withLock { $0 = EBADF }
                break
            }
            if descriptor.revents & Int16(POLLHUP | POLLERR) != 0 { break }
            let written = bytes.withUnsafeBytes { raw -> Int in
                guard let base = raw.baseAddress else { return 0 }
                return write(fd, base.advanced(by: offset), bytes.count - offset)
            }
            if written > 0 {
                offset += written
                continue
            }
            if written < 0 {
                if errno == EINTR || errno == EAGAIN { continue }
                if errno == EPIPE { break }
                error.withLock { $0 = errno }
            }
            break
        }
        close(fd)
    }

    // MARK: - Reading

    /// One pipe the pump reads. Internal, like the line parser below, so the parser's byte rules are unit-tested rather
    /// than inferred from a live pipe's chunking.
    struct Stream {
        let fd: Int32
        let cap: Int
        var kept = Data()
        var open = true
        /// Bytes of a line not yet ended, kept only for `onLine`.
        var pending: [UInt8] = []
        /// This line passed `lineCap`: its bytes are dropped until its newline arrives.
        var discarding = false

        init(_ fd: Int32, _ cap: Int) {
            self.fd = fd
            self.cap = cap
        }
    }

    /// **Both pipes, read to EOF on this one thread.** Non-blocking, so a `poll` that said there is something to read is
    /// always followed by a read that returns at once, and no other thread ever touches these descriptors: the stop
    /// flag ends the loop, and the thread closes what it still holds. Each stream keeps up to `cap` bytes and reads the
    /// rest and drops it, so the child can never block on a full pipe. A fatal `poll` or `read` marks the run failed —
    /// a broken stream is not an end of file.
    private static func pump(stdout outFD: Int32, stderr errFD: Int32, onLine: (@Sendable (String) -> Void)?,
                             stop: Locked<Bool>, failed: Locked<Bool>, out: Locked<String>, err: Locked<String>) {
        var streams = [Stream(outFD, stdoutCap), Stream(errFD, stderrCap)]
        for stream in streams where fcntl(stream.fd, F_SETFL, O_NONBLOCK) == -1 {
            failed.withLock { $0 = true }
            for open in streams { close(open.fd) }
            return
        }
        var buffer = [UInt8](repeating: 0, count: 65_536)
        while !stop.withLock({ $0 }), streams.contains(where: \.open) {
            var fds = streams.filter(\.open).map { pollfd(fd: $0.fd, events: Int16(POLLIN), revents: 0) }
            let ready = fds.withUnsafeMutableBufferPointer { poll($0.baseAddress, nfds_t($0.count), 100) }
            if ready < 0 {
                if errno == EINTR { continue }
                failed.withLock { $0 = true }
                break
            }
            var answer = 0
            for index in streams.indices where streams[index].open {
                defer { answer += 1 }
                guard ready > 0, fds[answer].revents != 0 else { continue }
                let count = buffer.withUnsafeMutableBytes { read(streams[index].fd, $0.baseAddress, $0.count) }
                if count > 0 {
                    let room = streams[index].cap - streams[index].kept.count
                    if room > 0 {
                        streams[index].kept.append(contentsOf: buffer[0..<min(count, room)])
                    }
                    if let onLine {
                        Self.lines(into: &streams[index], buffer[0..<count], onLine: onLine)
                    }
                    continue
                }
                if count == 0 {
                    close(streams[index].fd)
                    streams[index].open = false
                    continue
                }
                if errno == EINTR || errno == EAGAIN { continue }
                failed.withLock { $0 = true }
                close(streams[index].fd)
                streams[index].open = false
            }
        }
        for index in streams.indices where streams[index].open {
            // **Stopped before its end of file**: what was kept is not the child's whole output.
            failed.withLock { $0 = true }
            close(streams[index].fd)
            streams[index].open = false
        }
        if let onLine {
            for index in streams.indices where !streams[index].discarding {
                let line = String(decoding: streams[index].pending, as: UTF8.self)
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                if !line.isEmpty { onLine(line) }
            }
        }
        out.withLock { $0 = String(decoding: streams[0].kept, as: UTF8.self) }
        err.withLock { $0 = String(decoding: streams[1].kept, as: UTF8.self) }
    }

    /// **Bytes, not text**: a character split across two reads is decoded only once its line is whole. A line past
    /// `lineCap` is dropped through its newline, and the lines after it are still parsed.
    static func lines(into stream: inout Stream, _ bytes: ArraySlice<UInt8>, onLine: (String) -> Void) {
        stream.pending.append(contentsOf: bytes)
        // **One pass, one removal**: lines are read by index and the consumed prefix is cut once per chunk.
        var start = stream.pending.startIndex
        while let newline = stream.pending[start...].firstIndex(of: 0x0A) {
            let line = stream.pending[start..<newline]
            start = newline + 1
            if stream.discarding {
                stream.discarding = false
                continue
            }
            // **The cap covers a line completed by this very chunk**, not only one still waiting for its newline.
            if line.count > lineCap { continue }
            let text = String(decoding: line, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            if !text.isEmpty { onLine(text) }
        }
        stream.pending.removeSubrange(..<start)
        if stream.pending.count > lineCap {
            stream.pending.removeAll(keepingCapacity: true)
            stream.discarding = true
        }
    }

    // MARK: - Files

    static func isExecutable(_ url: URL) -> Bool {
        var info = stat()
        // **A regular file, not a directory that happens to be executable**: `access` alone would accept one.
        guard stat(url.path, &info) == 0, (info.st_mode & S_IFMT) == S_IFREG else { return false }
        return access(url.path, X_OK) == 0
    }

    private static func closeAll(_ pipes: [Int32]...) {
        for fd in pipes.joined() where fd >= 0 { close(fd) }
    }
}

/// Version strings as the CLIs print them (`2.1.304`, `codex-cli 0.163.0`): the first whitespace-separated token that
/// starts with a digit, its numeric components compared as integers, a non-numeric component as 0, a missing one as 0.
enum CLIVersion {
    static func atLeast(_ version: String, _ minimum: String) -> Bool {
        let a = components(version)
        let b = components(minimum)
        for index in 0..<max(a.count, b.count) {
            let left = index < a.count ? a[index] : 0
            let right = index < b.count ? b[index] : 0
            if left != right { return left > right }
        }
        return true
    }

    static func components(_ text: String) -> [Int] {
        let token = text.split(whereSeparator: \.isWhitespace).first { $0.first?.isNumber == true } ?? ""
        return token.split(separator: ".", omittingEmptySubsequences: false)
            .map { Int($0.prefix(while: \.isNumber)) ?? 0 }
    }
}
#endif
