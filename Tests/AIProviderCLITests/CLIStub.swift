import Foundation

/// **A stub CLI for the provider tests**: a script in a temporary folder that records what it was given — arguments,
/// environment, working directory, stdin — beside itself and answers from files the test writes. No real CLI, no
/// network, no sign-in. The folder is baked into the script because the child's environment is minimal by design.
struct CLIStub {
    let directory: URL

    init() throws {
        directory = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "sayso-ai-stub-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    }

    /// Writes an executable script named `name`.
    func script(_ name: String, body: String) throws -> URL {
        let url = directory.appending(path: name)
        try body.write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
        return url
    }

    /// The shared preamble: a counter, then argv, environment, cwd and stdin for this invocation. It ends with a
    /// newline of its own — a Swift multiline literal drops the one before its closing delimiter, and fragments that
    /// merely look like separate lines would otherwise be glued into one (`\"$D/pid.$n\"if [ …`).
    func recordingPreamble() -> String {
        let base = directory.path
        return """
        #!/bin/sh
        D='\(base)'
        n=$(( $(cat "$D/counter" 2>/dev/null || echo 0) + 1 ))
        echo "$n" > "$D/counter"
        printf '%s\\037' "$@" > "$D/argv.$n"
        env > "$D/env.$n"
        pwd > "$D/cwd.$n"
        cat > "$D/stdin.$n"
        echo $$ > "$D/pid.$n"
        """ + "\n"
    }

    /// The arguments, environment, working directory and stdin of invocation `n` (1-based).
    struct Invocation {
        let arguments: [String]
        let environment: [String: String]
        let workingDirectory: String
        let stdin: String
        let pid: pid_t
    }

    func invocation(_ n: Int) throws -> Invocation {
        // Arguments are separated by the ASCII unit separator, and empty ones are kept: `--tools ""` is an argument,
        // and a newline-separated record would lose it.
        var arguments = try String(contentsOf: directory.appending(path: "argv.\(n)"), encoding: .utf8)
            .split(separator: "\u{1f}", omittingEmptySubsequences: false).map(String.init)
        if arguments.last?.isEmpty == true { arguments.removeLast() }
        let environment = try String(contentsOf: directory.appending(path: "env.\(n)"), encoding: .utf8)
        var pairs: [String: String] = [:]
        for line in environment.split(separator: "\n") {
            guard let equals = line.firstIndex(of: "=") else { continue }
            pairs[String(line[..<equals])] = String(line[line.index(after: equals)...])
        }
        let cwd = try String(contentsOf: directory.appending(path: "cwd.\(n)"), encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let stdin = try String(contentsOf: directory.appending(path: "stdin.\(n)"), encoding: .utf8)
        let pidText = try String(contentsOf: directory.appending(path: "pid.\(n)"), encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return Invocation(arguments: arguments, environment: pairs, workingDirectory: cwd, stdin: stdin,
                          pid: pid_t(pidText) ?? 0)
    }

    /// Waits up to `seconds` for invocation `n` to have started (its pid file written). **Ten seconds by default, because
    /// the bound is paid only when something is wrong**: a stub starts in 50–270 ms on an idle host (the script's first
    /// run carries the system's one-time check of a new file), but the full suite runs its targets in parallel and a 2 s
    /// bound was outrun on mbp16 (2026-10-21: "the stub never started" on a test that had passed minutes before).
    func waitForStart(_ n: Int, seconds: Double = 10) async -> pid_t? {
        let deadline = Date().addingTimeInterval(seconds)
        while Date() < deadline {
            if let text = try? String(contentsOf: directory.appending(path: "pid.\(n)"), encoding: .utf8),
               let pid = pid_t(text.trimmingCharacters(in: .whitespacesAndNewlines)) {
                return pid
            }
            try? await Task.sleep(for: .milliseconds(20))
        }
        return nil
    }

    /// The child's `pwd` reports the physical path (`/private/var/…`) where the test built the logical one (`/var/…`):
    /// `/var` is a firmlink on this system. Comparing means dropping that prefix on either side.
    static func logical(_ path: String) -> String {
        path.hasPrefix("/private/") ? String(path.dropFirst("/private".count)) : path
    }

    func write(_ name: String, _ text: String) throws {
        try text.write(to: directory.appending(path: name), atomically: true, encoding: .utf8)
    }
}

/// True once `pid` names no process; `kill(pid, 0)` is the probe.
func isGone(_ pid: pid_t) -> Bool {
    guard pid > 0 else { return true }
    return kill(pid, 0) == -1 && errno == ESRCH
}

