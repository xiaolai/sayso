#if os(macOS)
import Foundation

/// Finding the Codex executable and preparing its environment (spec/credentials.md, executable and child).
public enum CodexExecutable {
    /// The folders searched after `PATH`, in order, for an app launched from the Finder, which has no login-shell
    /// `PATH`: Homebrew, /usr/local, the common per-user installers, then every nvm Node version, newest first.
    public static func candidates(home: String) -> [String] {
        var dirs = ["/opt/homebrew/bin", "/usr/local/bin"]
        // Joining would clean a NUL away ("/x\0/../y" is "/y"), so a home holding one adds nothing.
        guard !home.utf8.contains(0) else { return dirs }
        for rel in [".local/bin", ".npm-global/bin", ".volta/bin", ".bun/bin", "Library/pnpm"] {
            dirs.append(join(home, rel))
        }
        return dirs + nvmBins(home: home)
    }

    /// `~/.nvm/versions/node/*/bin`, newest version first by numeric comparison.
    private static func nvmBins(home: String) -> [String] {
        let root = join(home, ".nvm/versions/node")
        guard let names = try? FileManager.default.contentsOfDirectory(atPath: root) else { return [] }
        // A folder itself, not a link to one, as Go's DirEntry.IsDir reports it.
        let versions = names.filter { name in
            var st = stat()
            return lstat(join(root, name), &st) == 0 && st.st_mode & S_IFMT == S_IFDIR
        }
        // Go's strings.TrimPrefix, on bytes: a "v" before a combining mark is still the prefix.
        func bare(_ v: String) -> String { v.utf8.first == 0x76 ? String(decoding: v.utf8.dropFirst(), as: UTF8.self) : v }
        return versions.sorted { a, b in
            if versionLess(bare(b), bare(a)) { return true }
            if versionLess(bare(a), bare(b)) { return false }
            return a < b
        }.map { join(join(root, $0), "bin") }
    }

    /// The absolute path of the Codex executable: `explicit` when set (a path, or a name looked up in `path`),
    /// otherwise `codex` in `path`, then in each of `candidates`. An explicit choice that does not resolve is nil,
    /// never a fall-through. The result is absolute, a regular file, and executable.
    public static func resolve(explicit: String?, path: String, candidates: [String]) -> String? {
        // Split and searched as bytes: a separator followed by a combining mark is one Character but still a separator.
        let dirs = path.utf8.split(separator: 0x3A, omittingEmptySubsequences: false).map { String(decoding: $0, as: UTF8.self) }
        if let explicit, !explicit.isEmpty {
            if explicit.utf8.contains(0x2F) { return executable(explicit) }
            return look(in: dirs, for: explicit)
        }
        return look(in: dirs, for: "codex") ?? look(in: candidates, for: "codex")
    }

    private static func look(in dirs: [String], for name: String) -> String? {
        guard !name.utf8.contains(0) else { return nil }
        // A relative PATH entry would resolve against our working folder; a NUL would be cleaned away by joining.
        for d in dirs where d.utf8.first == 0x2F && !d.utf8.contains(0) {
            if let p = executable(join(d, name)) { return p }
        }
        return nil
    }

    /// The absolute, lexically cleaned form of `p` when it is a regular file this process may execute: an execute
    /// bit for another user only would stop the search at a file that cannot start. A NUL, which the system would
    /// read as the end of the path, matches nothing.
    private static func executable(_ p: String) -> String? {
        guard !p.utf8.contains(0) else { return nil }
        let abs = clean(p.utf8.first == 0x2F ? p : join(FileManager.default.currentDirectoryPath, p))
        var st = stat()
        guard stat(abs, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_mode & 0o111 != 0, access(abs, X_OK) == 0
        else { return nil }
        return abs
    }

    /// Whether dotted numeric version `a` is below `b`. Missing or non-numeric components count as zero; a component
    /// too large for an integer saturates, as Go's strconv.Atoi does.
    public static func versionLess(_ a: String, _ b: String) -> Bool {
        let x = a.split(separator: ".", omittingEmptySubsequences: false)
        let y = b.split(separator: ".", omittingEmptySubsequences: false)
        for i in 0..<max(x.count, y.count) {
            let p = i < x.count ? atoi(x[i]) : 0
            let q = i < y.count ? atoi(y[i]) : 0
            if p != q { return p < q }
        }
        return false
    }

    /// Go's strconv.Atoi: an optional sign and decimal digits, saturating at the integer bounds; anything else is 0.
    private static func atoi(_ s: Substring) -> Int {
        var digits = Substring(s)
        var negative = false
        if let f = digits.first, f == "+" || f == "-" {
            negative = f == "-"
            digits = digits.dropFirst()
        }
        guard !digits.isEmpty, digits.allSatisfy({ $0.isASCII && $0.isNumber }) else { return 0 }
        var n = 0
        for c in digits.utf8 {
            let d = Int(c - 0x30)
            let (m, o1) = n.multipliedReportingOverflow(by: 10)
            let (r, o2) = negative ? m.subtractingReportingOverflow(d) : m.addingReportingOverflow(d)
            if o1 || o2 { return negative ? Int.min : Int.max }
            n = r
        }
        return n
    }

    /// The variables that override Codex's own authentication or its token endpoints (upstream
    /// login/src/auth/manager.rs). They never reach the child; names compare case-insensitively.
    public static let removedEnvironment = [
        "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_APP_SERVER_LOGIN_CLIENT_ID",
        "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE",
    ]

    /// Prepended to the inherited `PATH`, after the executable's folder, so an npm-installed codex
    /// (`#!/usr/bin/env node`) finds its interpreter when the app was launched from the Finder.
    public static let repairPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

    /// `env` minus the authentication overrides, with `CODEX_HOME` replaced when `codexHome` is set and `PATH`
    /// repaired for `bin`. Nothing else is added.
    public static func childEnvironment(_ env: [String: String], bin: String, codexHome: String?) -> [String: String] {
        var out: [String: String] = [:]
        var inherited = ""
        let removed = Set(removedEnvironment.map { $0.uppercased() })
        for (name, value) in env {
            if removed.contains(name.uppercased()) { continue }
            if name == "PATH" {
                inherited = value
                continue
            }
            if let codexHome, !codexHome.isEmpty, name.uppercased() == "CODEX_HOME" { continue }
            out[name] = value
        }
        var path = dirname(bin) + ":" + repairPath
        if !inherited.isEmpty { path += ":" + inherited }
        out["PATH"] = path
        if let codexHome, !codexHome.isEmpty { out["CODEX_HOME"] = codexHome }
        return out
    }
}

func join(_ a: String, _ b: String) -> String { clean(a + "/" + b) }

/// Go's filepath.Dir.
func dirname(_ p: String) -> String {
    let c = Array(clean(p).utf8)
    guard let slash = c.lastIndex(of: 0x2F) else { return "." }
    return slash == 0 ? "/" : String(decoding: c[..<slash], as: UTF8.self)
}

/// Lexical cleaning exactly like Go's filepath.Clean, on bytes (a "/" before a combining mark still separates):
/// repeated separators become one, "." elements go, ".." removes the element before it, and a ".." that begins a
/// rooted path goes, while one that begins a relative path stays. An empty result is ".".
func clean(_ p: String) -> String {
    let bytes = Array(p.utf8)
    let rooted = bytes.first == 0x2F
    var parts: [ArraySlice<UInt8>] = []
    for part in bytes.split(separator: 0x2F) {
        if part.elementsEqual([0x2E]) { continue }
        if part.elementsEqual([0x2E, 0x2E]) {
            if let last = parts.last, !last.elementsEqual([0x2E, 0x2E]) {
                parts.removeLast()
            } else if !rooted {
                parts.append(part)
            }
            continue
        }
        parts.append(part)
    }
    let body = parts.map { String(decoding: $0, as: UTF8.self) }.joined(separator: "/")
    return rooted ? "/" + body : (body.isEmpty ? "." : body)
}
#endif
