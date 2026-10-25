#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// Writes `auth.json` into `codexHome`, a folder made for a Codex of the caller's own, from `token`, a token a fetch
/// or refresh returned (spec/credentials.md, AUTH-08). The file signs that Codex in to the ChatGPT account the token
/// names, with no refresh token, and is stamped as refreshed at `now`. The user's own auth.json is never read.
///
/// `codexHome` must be an absolute path to a folder the user owns that grants nothing to anyone else (no group or
/// other permission bits); a link to one is followed once, and the folder is then opened along the resolved path
/// without following a link. `auth.json` must not exist there. Every failure is a ``CodexAuthError`` in the
/// ``CodexAuthState/codexError`` state, and leaves no auth.json behind. The bytes are built in memory of their own,
/// wiped once written.
///
/// The token's freshness is the caller's: one the caller fetched with the margin its Codex needs, not one already
/// expired at `now`, which is refused.
public func writePrivateSignIn(_ token: Token, codexHome: String, now: Date = Date()) throws {
    let bearer = token.bearer
    guard jwtShaped(bearer) else {
        throw CodexAuthError(.codexError, message: "the token is not a JWT, so a private Codex cannot read it")
    }
    let claims = jwtClaims(bearer)
    guard let account = claims.account else {
        throw CodexAuthError(.codexError, message: "the token names no ChatGPT account")
    }
    if let exp = claims.expires, exp <= now { throw CodexAuthError(.codexError, message: "the token has expired") }
    let stamp = try utcStamp(now)
    let dir = try openPrivateHome(codexHome)
    defer { Darwin.close(dir) }

    let parts = [
        #"{"auth_mode":"chatgpt","last_refresh":""#, stamp, #"","tokens":{"id_token":""#, bearer,
        #"","access_token":""#, bearer, #"","refresh_token":"","account_id":""#, account, #""}}"#,
    ]
    // Memory of the exact size, wiped when released, so no byte of the token written outlives the write.
    let file = OwnedBytes(count: parts.reduce(0) { $0 + $1.utf8.count })
    var k = 0
    for part in parts {
        for b in part.utf8 {
            file.bytes[k] = b
            k += 1
        }
    }
    try withExtendedLifetime(file) { try writeNew(dir, UnsafeRawBufferPointer(file.bytes)) }
}

/// Whether `s` is three base64url segments joined by dots, each non-empty: a token Codex can read as a JWT, and one
/// written into JSON as it is, with nothing to escape.
func jwtShaped(_ s: String) -> Bool {
    var segments = 1, length = 0
    for c in s.utf8 {
        if c == UInt8(ascii: ".") {
            guard length > 0 else { return false }
            segments += 1
            length = 0
        } else {
            guard base64URL(c) != nil else { return false }
            length += 1
        }
    }
    return segments == 3 && length > 0
}

/// `now` in UTC as RFC 3339 whole seconds, `2027-01-15T08:00:00Z`; a fraction of a second is dropped, as Go's
/// formatting drops it. An instant outside the years 0000 to 9999, which RFC 3339 cannot write, is refused.
private func utcStamp(_ now: Date) throws -> String {
    let seconds = now.timeIntervalSince1970.rounded(.down)
    // 0000-01-01T00:00:00Z and 9999-12-31T23:59:59Z; NaN fails both.
    guard seconds >= -62_167_219_200, seconds <= 253_402_300_799 else {
        throw CodexAuthError(.codexError, message: "the sign-in time cannot be written as an RFC 3339 timestamp")
    }
    var t = time_t(seconds)
    var parts = tm()
    guard gmtime_r(&t, &parts) != nil else {
        throw CodexAuthError(.codexError, message: "the sign-in time cannot be written as an RFC 3339 timestamp")
    }
    return String(
        format: "%04d-%02d-%02dT%02d:%02d:%02dZ", parts.tm_year + 1900, parts.tm_mon + 1, parts.tm_mday, parts.tm_hour,
        parts.tm_min, parts.tm_sec)
}

/// Resolves `codexHome` once and opens the folder along the resolved path without following a link, then checks the
/// open folder: a folder, owned by the user, granting nothing to anyone else. The caller closes it.
private func openPrivateHome(_ codexHome: String) throws -> Int32 {
    guard !codexHome.utf8.contains(0), codexHome.utf8.first == UInt8(ascii: "/") else {
        throw CodexAuthError(.codexError, message: "the private Codex home is not an absolute path")
    }
    guard let real = AuthFile.realPath(codexHome) else {
        throw CodexAuthError(.codexError, message: "the private Codex home does not resolve")
    }
    guard let dir = AuthFile.openResolved(real) else {
        throw CodexAuthError(
            .codexError, message: "the private Codex home is not a folder, or a folder of its path changed")
    }
    var st = stat()
    let problem: String? =
        if fstat(dir, &st) != 0 || st.st_mode & S_IFMT != S_IFDIR {
            "the private Codex home is not a folder"
        } else if st.st_uid != getuid() {
            "the private Codex home is not owned by the user"
        } else if st.st_mode & 0o077 != 0 {
            "the private Codex home grants access to other users"
        } else {
            nil
        }
    if let problem {
        Darwin.close(dir)
        throw CodexAuthError(.codexError, message: problem)
    }
    return dir
}

/// Creates auth.json in `dir`, new (an entry of that name already there, a link included, is refused rather than
/// replaced or followed), mode 0600 whatever the umask, and writes `data` to it in full; on any failure the file it
/// created is removed.
private func writeNew(_ dir: Int32, _ data: UnsafeRawBufferPointer) throws {
    let flags = O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC
    let fd = AuthFile.openRetrying { openat(dir, AuthFile.fileName, flags, 0o600) }
    if fd < 0 {
        if errno == EEXIST {
            throw CodexAuthError(.codexError, message: "the private Codex home already holds an auth.json")
        }
        throw CodexAuthError(
            .codexError, message: "auth.json cannot be created in the private Codex home",
            cause: String(cString: strerror(errno)))
    }
    // A removal that fails leaves token bytes behind and blocks the next attempt (EEXIST), so it is reported.
    func removed(_ message: String, _ code: Int32) -> CodexAuthError {
        var rc: Int32
        repeat { rc = unlinkat(dir, AuthFile.fileName, 0) } while rc != 0 && errno == EINTR
        guard rc == 0 else {
            let unlinkCode = errno
            return CodexAuthError(
                .codexError, message: message + ", and the partial auth.json could not be removed",
                cause: String(cString: strerror(code)) + "; removing: " + String(cString: strerror(unlinkCode)))
        }
        return CodexAuthError(.codexError, message: message, cause: String(cString: strerror(code)))
    }
    guard fchmod(fd, 0o600) == 0 else {
        let code = errno
        Darwin.close(fd)
        throw removed("auth.json cannot be made private", code)
    }
    var written = 0
    while written < data.count {
        let n = Darwin.write(fd, data.baseAddress! + written, data.count - written)
        if n < 0, errno == EINTR { continue }
        guard n > 0 else {  // a regular file never takes nothing; never spin on it
            let code = n < 0 ? errno : EIO
            Darwin.close(fd)
            throw removed("auth.json cannot be written", code)
        }
        written += n
    }
    guard Darwin.close(fd) == 0 else { throw removed("auth.json cannot be written", errno) }
}
#endif
