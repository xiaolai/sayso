import Foundation
import SaySoAI
import Testing
@testable import SaySoAICLI

/// **The child's pipes under stress** (ADR-0021 d.9; audit findings of 2026-10-20): a CLI that exits before reading its
/// prompt must not take the host app down with SIGPIPE, and a stream with no newline must not grow the pending line without
/// bound.
struct CLIProcessTests {
    static func directory() throws -> URL {
        let url = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "sayso-cli-process-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    static func script(_ body: String, in directory: URL) throws -> URL {
        let url = directory.appending(path: "child")
        try body.write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
        return url
    }

    /// A child that exits at once, with a megabyte waiting on stdin: `write` meets a closed pipe. Without
    /// `F_SETNOSIGPIPE` this test process dies on SIGPIPE and never reports.
    @Test func aChildThatExitsBeforeReadingDoesNotSignalTheApp() async throws {
        let directory = try Self.directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let executable = try Self.script("#!/bin/sh\nexit 0\n", in: directory)
        let run = try await CLIProcess.run(
            executable: executable, arguments: [], environment: ["PATH": "/usr/bin:/bin"],
            workingDirectory: directory, input: String(repeating: "x", count: 1 << 20), deadline: .seconds(10))
        #expect(run.status == 0)
    }

    /// **Concurrency must not exhaust the process's descriptors** (2026-10-23): the readers were blocking work on the
    /// shared dispatch pool; under 32 concurrent runs the pool starved, the reader join timed out, and each abandoned
    /// join left two descriptors open until the 2,560-descriptor soft limit — where `posix_spawn` itself starts failing
    /// with EMFILE (`spawn(24)`). That is how the CLI provider tests went red under a loaded suite: clean at 4
    /// concurrent, EMFILE by the 43rd iteration at 32.
    @Test func manyConcurrentRunsKeepTheirDescriptors() async throws {
        let before = Self.openPipes()
        let failures = await withTaskGroup(of: Int.self) { group in
            for _ in 0..<32 {
                group.addTask {
                    var failed = 0
                    for _ in 0..<100 {
                        do {
                            _ = try await CLIProcess.run(
                                executable: URL(filePath: "/bin/echo"), arguments: ["x"],
                                environment: ["PATH": "/usr/bin:/bin"], workingDirectory: URL(filePath: "/tmp"),
                                deadline: .seconds(10))
                        } catch {
                            failed += 1
                        }
                    }
                    return failed
                }
            }
            var total = 0
            for await failed in group { total += failed }
            return total
        }
        #expect(failures == 0, "\(failures) runs could not start")
        // **Only pipes are counted, and a sibling suite's pipes are given a bounded moment to close** (2026-10-24: the
        // process-wide count read 12 → 47 while an HTTP suite's sockets were open). This test's leak is a pipe pair per
        // abandoned child, held for good; a sibling's pipes are transient. The bound is unchanged.
        let settle = ContinuousClock.now + .seconds(2)
        while Self.openPipes() > before + 8, ContinuousClock.now < settle {
            try? await Task.sleep(for: .milliseconds(20))
        }
        #expect(Self.openPipes() <= before + 8, "pipes: \(before) → \(Self.openPipes())")
    }

    /// **The descriptors this test can leak are pipes** — one pair per child's stdin and stdout (`CLIProcess`). Counting
    /// pipes keeps another suite's sockets and files out of a count that is the whole process's; counting every
    /// descriptor measured the HTTP suite beside this one (12 → 47, 2026-10-24).
    static func openPipes() -> Int {
        let stride = MemoryLayout<proc_fdinfo>.stride
        let needed = proc_pidinfo(getpid(), PROC_PIDLISTFDS, 0, nil, 0)
        guard needed > 0 else { return -1 }
        var entries = [proc_fdinfo](repeating: proc_fdinfo(), count: Int(needed) / stride)
        let got = entries.withUnsafeMutableBytes { buffer in
            proc_pidinfo(getpid(), PROC_PIDLISTFDS, 0, buffer.baseAddress, Int32(buffer.count))
        }
        guard got > 0 else { return -1 }
        return entries.prefix(Int(got) / stride).filter { $0.proc_fdtype == UInt32(PROX_FDTYPE_PIPE) }.count
    }

    /// **Bytes, not text**: a character split across two reads is decoded only once its line is whole.
    @Test func aCharacterSplitAcrossReadsDecodesOnlyWhenWhole() {
        var stream = CLIProcess.Stream(0, 1024)
        var lines: [String] = []
        let bytes = Array("a\u{20AC}\n".utf8)
        CLIProcess.lines(into: &stream, bytes[0..<1], onLine: { lines.append($0) })
        CLIProcess.lines(into: &stream, bytes[1..<3], onLine: { lines.append($0) })
        CLIProcess.lines(into: &stream, bytes[3...], onLine: { lines.append($0) })
        #expect(lines == ["a\u{20AC}"])
        #expect(!lines.contains { $0.contains("\u{FFFD}") })
    }

    /// **An overlong line does not swallow the lines after it**: its bytes are dropped through its newline, and parsing
    /// resumes on the next line.
    @Test func anOverlongLineIsDroppedWithoutLosingTheNextOne() {
        var stream = CLIProcess.Stream(0, 1024)
        var lines: [String] = []
        let overlong = Array(String(repeating: "x", count: CLIProcess.lineCap + 10).utf8)
        CLIProcess.lines(into: &stream, overlong[...], onLine: { lines.append($0) })
        CLIProcess.lines(into: &stream, Array("tail\nnext\n".utf8)[...], onLine: { lines.append($0) })
        #expect(lines == ["next"])
    }

    /// **An overlong line that is completed by the same chunk is dropped too**, and the line after it is kept.
    @Test func anOverlongLineEndingInThisChunkIsDropped() {
        var stream = CLIProcess.Stream(0, 1024)
        var lines: [String] = []
        let bytes = Array((String(repeating: "x", count: CLIProcess.lineCap + 10) + "\nnext\n").utf8)
        CLIProcess.lines(into: &stream, bytes[...], onLine: { lines.append($0) })
        #expect(lines == ["next"])
    }

    /// **A transport failure is not a model refusal**, and the word "oauth" in prose is not a sign-in failure.
    @Test func aConnectionRefusalIsNotAModelRefusal() {
        #expect(CLIProviderSupport.classify("dial tcp: connection refused", provider: "codex", status: 1)
                == .framework("codex exit 1"))
        #expect(CLIProviderSupport.classify("OAuth is documented at example.invalid", provider: "claude", status: 1)
                == .framework("claude exit 1"))
        #expect(CLIProviderSupport.classify("the model refused the request", provider: "claude", status: 1)
                == .refused)
        #expect(CLIProviderSupport.classify("dial tcp: connection declined", provider: "codex", status: 1)
                == .framework("codex exit 1"))
        #expect(CLIProviderSupport.classify("sandbox: approval declined", provider: "codex", status: 1)
                == .framework("codex exit 1"))
        #expect(CLIProviderSupport.classify("Not signed in. Please run /login.", provider: "claude", status: 1)
                == .notSignedIn)
    }

    /// The printed forms both CLIs use: `2.1.304` and `codex-cli 0.163.0`.
    @Test func aPrefixedVersionStringIsRead() {
        #expect(CLIVersion.atLeast("codex-cli 0.163.0", "0.163.0"))
        #expect(!CLIVersion.atLeast("codex-cli 0.162.9", "0.163.0"))
        #expect(CLIVersion.atLeast("2.1.304 (Claude Code)", "2.1.304"))
        #expect(!CLIVersion.atLeast("2.1.303", "2.1.304"))
    }

    /// **The run owns its process group**: a descendant the launcher leaves behind is killed with the run, not left
    /// holding the pipes.
    @Test func aDescendantOutlivingTheLauncherIsKilled() async throws {
        let directory = try Self.directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let body = "#!/bin/sh\nsleep 30 &\necho $! > \"$(dirname \"$0\")/grandchild\"\necho done\nexit 0\n"
        let executable = try Self.script(body, in: directory)
        let run = try await CLIProcess.run(
            executable: executable, arguments: [], environment: ["PATH": "/usr/bin:/bin"],
            workingDirectory: directory, input: nil, deadline: .seconds(10))
        #expect(run.status == 0)
        let text = try String(contentsOf: directory.appending(path: "grandchild"), encoding: .utf8)
        let pid = pid_t(text.trimmingCharacters(in: .whitespacesAndNewlines)) ?? 0
        #expect(pid > 0)
        #expect(await eventuallyOffMain { isGone(pid) }, "the descendant outlived the run (pid \(pid))")
    }

    /// A megabyte with no newline: the pending line is dropped rather than held, and the run still ends.
    @Test func aNewlineFreeStreamDoesNotGrowThePendingLine() async throws {
        let directory = try Self.directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let body = "#!/bin/sh\nhead -c 1048576 /dev/zero | tr '\\0' x\nexit 0\n"
        let executable = try Self.script(body, in: directory)
        let lines = Locked(0)
        let run = try await CLIProcess.run(
            executable: executable, arguments: [], environment: ["PATH": "/usr/bin:/bin"],
            workingDirectory: directory, input: nil, deadline: .seconds(20),
            onLine: { _ in lines.withLock { $0 += 1 } })
        #expect(run.status == 0)
        #expect(run.stdout.utf8.count <= CLIProcess.stdoutCap)
    }
}
