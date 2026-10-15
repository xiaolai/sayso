#if os(macOS)
import Foundation

/// Finding the Codex executable and preparing its environment (spec/credentials.md, executable and child).
public enum CodexExecutable {
    /// The folders searched after `PATH`, in order, for an app launched from the Finder, which has no login-shell
    /// `PATH`: Homebrew, /usr/local, the common per-user installers, then every nvm Node version, newest first.
    public static func candidates(home: String) -> [String] {
        var dirs = ["/opt/homebrew/bin", "/usr/local/bin"]
        for rel in [".local/bin", ".npm-global/bin", ".volta/bin", ".bun/bin", "Library/pnpm"] {
            dirs.append(join(home, rel))
        }
        return dirs + nvmBins(home: home)
    }

    /// `~/.nvm/versions/node/*/bin`, newest version first by numeric comparison.
    private static func nvmBins(home: String) -> [String] {
        let root = join(home, ".nvm/versions/node")
        guard let names = try? FileManager.default.contentsOfDirectory(atPath: root) else { return [] }
        let versions = names.filter { name in
            var isDir: ObjCBool = false
            return FileManager.default.fileExists(atPath: join(root, name), isDirectory: &isDir) && isDir.boolValue
        }
        func bare(_ v: String) -> String { v.hasPrefix("v") ? String(v.dropFirst()) : v }
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
        let dirs = path.split(separator: ":", omittingEmptySubsequences: false).map(String.init)
        if let explicit, !explicit.isEmpty {
            if explicit.contains("/") { return executable(explicit) }
            return look(in: dirs, for: explicit)
        }
        return look(in: dirs, for: "codex") ?? look(in: candidates, for: "codex")
    }

    private static func look(in dirs: [String], for name: String) -> String? {
        // A relative PATH entry would resolve against our working folder.
        for d in dirs where d.hasPrefix("/") {
            if let p = executable(join(d, name)) { return p }
        }
        return nil
    }

    /// The absolute, lexically cleaned form of `p` when it is a regular file with an execute bit.
    private static func executable(_ p: String) -> String? {
        let abs = clean(p.hasPrefix("/") ? p : join(FileManager.default.currentDirectoryPath, p))
        var st = stat()
        guard stat(abs, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_mode & 0o111 != 0 else { return nil }
        return abs
    }

    /// Whether dotted numeric version `a` is below `b`. Missing or non-numeric components count as zero.
    public static func versionLess(_ a: String, _ b: String) -> Bool {
        let x = a.split(separator: ".", omittingEmptySubsequences: false)
        let y = b.split(separator: ".", omittingEmptySubsequences: false)
        for i in 0..<max(x.count, y.count) {
            let p = i < x.count ? Int(x[i]) ?? 0 : 0
            let q = i < y.count ? Int(y[i]) ?? 0 : 0
            if p != q { return p < q }
        }
        return false
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

func dirname(_ p: String) -> String {
    let c = clean(p)
    guard let slash = c.lastIndex(of: "/") else { return "." }
    return slash == c.startIndex ? "/" : String(c[..<slash])
}

/// Lexical cleaning like Go's filepath.Clean, for absolute paths.
func clean(_ p: String) -> String {
    var parts: [Substring] = []
    for part in p.split(separator: "/") {
        switch part {
        case ".": continue
        case "..": if !parts.isEmpty { parts.removeLast() }
        default: parts.append(part)
        }
    }
    let body = parts.joined(separator: "/")
    return p.hasPrefix("/") ? "/" + body : (body.isEmpty ? "." : body)
}
#endif
