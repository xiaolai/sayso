#if os(macOS)
import Foundation
import SaySoAI

/// **What both CLI providers do the same way**: map a process failure, make the app's own folders, spend one deadline
/// across a probe and an ask, and read the words a CLI uses for the same refusal.
enum CLIProviderSupport {
    static func mapped(_ failure: CLIProcess.Failure) -> AIProviderError {
        switch failure {
        case .notInstalled: .notInstalled
        case .timeout: .timeout
        case .cancelled: .cancelled
        case .readerFailed: .framework("reader")
        case .spawn(let code): .framework("spawn \(code)")
        }
    }

    /// **The app's own folders, created private and then checked.** A folder that already exists may be a symlink, be
    /// owned by somebody else, or be writable by others, and the CLI writes a credential into one of these; the check is
    /// a refusal, never a chmod of a folder the app did not make.
    /// **The path to each folder is checked too**, before anything is created and again after: a parent others may
    /// write to would let them replace the folder after its own check.
    static func prepare(_ folders: [URL]) throws(AIProviderError) {
        for folder in folders {
            guard isProtectedPath(folder.path) else { throw .framework("folder") }
            do {
                try FileManager.default.createDirectory(
                    at: folder, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            } catch {
                throw .framework("folder")
            }
            guard isProtectedPath(folder.path), isPrivate(folder) else { throw .framework("folder") }
        }
    }

    /// One `lstat`: a real folder, not a symlink to one (the CLI would write outside the app's tree), this user's, and
    /// writable by nobody else.
    static func isPrivate(_ folder: URL) -> Bool {
        var info = stat()
        guard lstat(folder.path, &info) == 0, (info.st_mode & S_IFMT) == S_IFDIR else { return false }
        return info.st_uid == getuid() && info.st_mode & mode_t(0o022) == 0
    }

    /// **Nobody else can change where `path` leads.** Every folder on the way, symlinks followed, is root's or this
    /// user's and writable by nobody else — a sticky folder such as `/tmp` excepted, where others may add an entry but
    /// never rename or remove ours. A symlink on the way is root's or this user's and sits in such a folder, so nobody
    /// else can re-point it. A component not there yet ends the walk: what this process creates is its own.
    static func isProtectedPath(_ path: String) -> Bool {
        guard path.hasPrefix("/") else { return false }
        var current = ""
        for component in path.split(separator: "/") where component != "." {
            guard component != ".." else { return false }
            let next = current + "/" + component
            var info = stat()
            guard lstat(next, &info) == 0 else { return errno == ENOENT }
            guard info.st_uid == 0 || info.st_uid == getuid() else { return false }
            switch info.st_mode & S_IFMT {
            case S_IFDIR:
                guard info.st_mode & mode_t(0o022) == 0 || info.st_mode & S_ISVTX != 0 else { return false }
                current = next
            case S_IFLNK:
                guard let real = realpath(next, nil) else { return false }
                let resolved = String(cString: real)
                free(real)
                guard isProtectedPath(resolved) else { return false }
                current = resolved == "/" ? "" : resolved
            default:
                return false
            }
        }
        return true
    }

    /// The time left on the ask after a probe has already spent some of it; `.timeout` once nothing is left.
    static func remaining(until deadline: ContinuousClock.Instant,
                          clock: ContinuousClock) throws(AIProviderError) -> Duration {
        let left = deadline - clock.now
        guard left > .zero else { throw .timeout }
        return left
    }

    /// The environment a child starts from, before a provider adds its own folder: the app's home, the temporary
    /// folder, and a `PATH` that includes the standard installer folders — an app launched from the Finder has no
    /// login-shell `PATH`, and an npm-installed CLI needs `node` beside it.
    static func baseEnvironment() -> [String: String] {
        var environment = ["HOME": NSHomeDirectory()]
        var path = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
        if let inherited = ProcessInfo.processInfo.environment["PATH"], !inherited.isEmpty {
            path = inherited + ":" + path
        }
        environment["PATH"] = path
        if let temporary = ProcessInfo.processInfo.environment["TMPDIR"] { environment["TMPDIR"] = temporary }
        return environment
    }

    /// The words a CLI uses when it cannot answer, for the error that names why. **Markers are narrow**: a sentence
    /// about OAuth documentation, an approval policy or a safety folder is not a sign-in failure or a refusal.
    static func classify(_ text: String, provider: String, status: Int32,
                         signIn extras: [String] = []) -> AIProviderError {
        let lower = text.lowercased()
        let signIn = ["not signed in", "please run /login", "please log in", "authentication required",
                      "invalid api key", "unauthorized"] + extras
        if signIn.contains(where: { lower.contains($0) }) { return .notSignedIn }
        let throttled = ["rate limit", "usage limit", "quota exceeded", "too many requests"]
        if throttled.contains(where: { lower.contains($0) }) { return .rateLimited }
        // **A model's refusal, never a connection's**: "connection refused" and "refused to connect" are transport
        // failures, and the marker list keeps only the phrases a model or a filter uses about an answer. A policy or a
        // filter named alone is not a refusal ("failed to initialize safety filter" is a broken CLI): the phrase must say
        // the request was declined.
        let refused = ["model refused", "refused to answer", "model declined", "declined to answer",
                       "violates our usage policy", "violates our content policy", "violates the usage policy",
                       "violates the content policy", "blocked by the safety filter", "blocked by the content filter"]
        if refused.contains(where: { lower.contains($0) }) { return .refused }
        // **An error that names nothing this build knows is not a refusal by default**: an in-envelope error (status 0)
        // says only that the CLI failed, and a failing exit's status is the only part safe to show. A refusal is
        // claimed only when the text says so.
        return status == 0 ? .framework("\(provider) error") : .framework("\(provider) exit \(status)")
    }
}
#endif
