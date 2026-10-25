#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// Where auth.json is read: a pinned, validated CODEX_HOME folder, or, with CODEX_HOME unset, the default path. The
/// folder stays open for every read, so replacing a folder of the path after validation (with a link elsewhere, say)
/// cannot redirect a later read. The caller closes it.
struct AuthLocation {
    /// The open folder; -1 for the default path.
    let dir: Int32
    /// The file, for the caller's guard and for diagnostics.
    let path: String

    func close() {
        if dir >= 0 { Darwin.close(dir) }
    }
}

/// The file source (spec/credentials.md): `auth.json`, read without following a link, checked on the open
/// descriptor, bounded, never written.
enum AuthFile {
    /// The size from which auth.json is refused.
    static let maxBytes = 64 << 10
    /// How long the one re-read after a parse failure waits for a writer to finish.
    static let tornRetryDelay: TimeInterval = 0.1
    static let fileName = "auth.json"

    /// Locates `$CODEX_HOME/auth.json` when `codexHome` resolves, after following links, to a folder the user owns,
    /// inside `confine` when that is set; `~/.codex/auth.json` when `codexHome` is unset. Both are resolved once and
    /// the folder is then opened along the resolved path. A set CODEX_HOME that fails is codex-error.
    static func location(codexHome: String?, home: String, confine: String?) throws -> AuthLocation {
        guard let codexHome, !codexHome.isEmpty else { return AuthLocation(dir: -1, path: join(home, ".codex/auth.json")) }
        guard !codexHome.utf8.contains(0), !(confine ?? "").utf8.contains(0) else {
            throw CodexAuthError(.codexError, message: "CODEX_HOME is not a valid path")
        }
        guard codexHome.utf8.first == 0x2F else { throw CodexAuthError(.codexError, message: "CODEX_HOME is not an absolute path") }
        guard let real = realPath(codexHome) else {
            throw CodexAuthError(.codexError, message: "CODEX_HOME does not resolve")
        }
        var root: FolderID?
        if let confine, !confine.isEmpty {
            var st = stat()
            guard stat(confine, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else {
                throw CodexAuthError(.codexError, message: "the folder CODEX_HOME is confined to does not resolve")
            }
            root = FolderID(dev: st.st_dev, ino: st.st_ino)
        }
        // The resolved path holds no link, so it is opened from "/" one folder at a time, never following one: a
        // folder of the path replaced since it was resolved (by a link elsewhere, say) fails the open instead of
        // redirecting it. It is inside the confinement when one of the folders opened is the confinement's folder,
        // compared by device and inode as in Go, never by path: the check is on the folders the read goes through,
        // and a path reaching the folder through another mount (a firmlink, say) names the same folder.
        guard let (dir, inside) = openResolved(real, through: root) else {
            throw CodexAuthError(
                .codexError,
                message: root != nil
                    ? "CODEX_HOME is not a folder inside the folder it is confined to" : "CODEX_HOME is not a folder")
        }
        guard root == nil || inside else {
            Darwin.close(dir)
            throw CodexAuthError(.codexError, message: "CODEX_HOME is outside the folder it is confined to")
        }
        // Checked on the open folder, the one every read goes through.
        var st = stat()
        guard fstat(dir, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else {
            Darwin.close(dir)
            throw CodexAuthError(.codexError, message: "CODEX_HOME is not a folder")
        }
        guard st.st_uid == getuid() else {
            Darwin.close(dir)
            throw CodexAuthError(.codexError, message: "CODEX_HOME is not owned by the user")
        }
        return AuthLocation(dir: dir, path: join(real, fileName))
    }

    /// A folder's identity, the same at every path that reaches it.
    struct FolderID: Equatable {
        let dev: dev_t
        let ino: ino_t
    }

    /// As ``openResolved(_:through:)``, without the identity check.
    static func openResolved(_ path: String) -> Int32? { openResolved(path, through: nil)?.fd }

    /// The folder at `path`, an absolute path holding no link, opened from "/" one component at a time without
    /// following a link and without blocking on a FIFO, each component checked to be a folder on its own descriptor;
    /// nil on failure. Each folder is opened for search only (O_SEARCH): walking a path needs search permission on
    /// its folders, not read permission, so an ancestor such as a 0711 home folder does not refuse it. `through` is
    /// whether one of the folders opened, "/" and the last included, is the folder `via`; false when `via` is nil.
    static func openResolved(_ path: String, through via: FolderID?) -> (fd: Int32, through: Bool)? {
        let flags = O_SEARCH | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC
        guard path.utf8.first == 0x2F, !path.utf8.contains(0) else { return nil }
        var fd = openRetrying { open("/", flags) }
        guard fd >= 0 else { return nil }
        var st = stat()
        guard fstat(fd, &st) == 0 else {
            Darwin.close(fd)
            return nil
        }
        var through = via == FolderID(dev: st.st_dev, ino: st.st_ino)
        for part in path.utf8.split(separator: 0x2F).map({ String(decoding: $0, as: UTF8.self) }) {
            guard part != ".", part != ".." else {  // a resolved path has neither; refused rather than walked
                Darwin.close(fd)
                return nil
            }
            let next = openRetrying { openat(fd, part, flags) }
            Darwin.close(fd)
            guard next >= 0 else { return nil }
            guard fstat(next, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else {
                Darwin.close(next)
                return nil
            }
            through = through || via == FolderID(dev: st.st_dev, ino: st.st_ino)
            fd = next
        }
        return (fd, through)
    }

    /// Runs `open` until it is not interrupted: a signal arriving during an open is no reason to refuse the file.
    /// errno is the call's own when it returns.
    static func openRetrying(_ open: () -> Int32) -> Int32 {
        while true {
            let fd = interruptions.take() ? { errno = EINTR; return -1 }() : open()
            if fd >= 0 || errno != EINTR { return fd }
        }
    }

    /// For tests: how many opens still fail with EINTR before the real call runs.
    static let interruptions = Interruptions()

    final class Interruptions: @unchecked Sendable {
        private let lock = NSLock()
        private var left = 0

        func set(_ n: Int) { lock.withLock { left = n } }

        func take() -> Bool {
            lock.withLock {
                guard left > 0 else { return false }
                left -= 1
                return true
            }
        }
    }

    static func realPath(_ p: String) -> String? {
        guard let r = realpath(p, nil) else { return nil }
        defer { free(r) }
        return String(cString: r)
    }

    /// The ChatGPT token the file at `loc` holds. A missing file is no-sign-in-file; a parse failure is retried
    /// once after `onRetry` and a short delay.
    static func read(_ loc: AuthLocation, onRetry: (@Sendable () -> Void)?) throws -> Token {
        if let parsed = try readOnce(loc) { return parsed }
        onRetry?()
        Thread.sleep(forTimeInterval: tornRetryDelay)
        if let parsed = try readOnce(loc) { return parsed }
        throw CodexAuthError(.codexError, message: "auth.json is not a valid sign-in file")
    }

    /// Opens the file without following a link and without blocking on a FIFO, checks the open descriptor, and
    /// decodes the bytes read from it. nil only when the content did not decode.
    private static func readOnce(_ loc: AuthLocation) throws -> Token? {
        guard !loc.path.utf8.contains(0) else {
            throw CodexAuthError(.codexError, message: "auth.json cannot be opened", cause: "the path holds a NUL")
        }
        let flags = O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC
        let fd = openRetrying { loc.dir >= 0 ? openat(loc.dir, fileName, flags) : open(loc.path, flags) }
        if fd < 0 {
            if errno == ENOENT { throw CodexAuthError(.noSignInFile, message: "auth.json does not exist") }
            throw CodexAuthError(.codexError, message: "auth.json cannot be opened", cause: String(cString: strerror(errno)))
        }
        defer { Darwin.close(fd) }
        var st = stat()
        guard fstat(fd, &st) == 0 else { throw CodexAuthError(.codexError, message: "auth.json cannot be inspected") }
        guard st.st_mode & S_IFMT == S_IFREG else {
            throw CodexAuthError(.codexError, message: "auth.json is not a regular file")
        }
        guard st.st_uid == getuid() else { throw CodexAuthError(.codexError, message: "auth.json is not owned by the user") }
        guard st.st_size < maxBytes else { throw CodexAuthError(.codexError, message: "auth.json is 64 KiB or larger") }

        var buf = [UInt8](repeating: 0, count: maxBytes)
        defer { buf.withUnsafeMutableBytes { _ = memset_s($0.baseAddress, $0.count, 0, $0.count) } }  // every field, any API key
        var used = 0
        while used < maxBytes {
            let n = buf.withUnsafeMutableBytes { Darwin.read(fd, $0.baseAddress! + used, maxBytes - used) }
            if n < 0 {
                if errno == EINTR { continue }
                throw CodexAuthError(.codexError, message: "auth.json cannot be read")
            }
            if n == 0 { break }
            used += n
        }
        guard used < maxBytes else { throw CodexAuthError(.codexError, message: "auth.json is 64 KiB or larger") }
        // Read where it lies, in `buf`, which is wiped on every exit: no check or lookup copies any of it.
        return try buf.withUnsafeBytes { all in try decode(borrowing: UnsafeRawBufferPointer(rebasing: all[0..<used])) }
    }

    /// The auth_mode values Codex knows (protocol/src/auth.rs), other than chatgpt and apikey. Compared as bytes.
    private static let fileModes = [
        "chatgptAuthTokens", "headers", "agentIdentity", "personalAccessToken", "bedrockApiKey", "bedrockAccessKeys",
    ]

    /// ``decode(borrowing:)`` of `data`'s bytes, in place.
    static func decode(_ data: Data) throws -> Token? { try data.withUnsafeBytes { try decode(borrowing: $0) } }

    /// Resolves the sign-in mode in Codex's own order (AuthDotJson::resolved_mode) and returns the ChatGPT token; nil
    /// when `raw` is not one plain JSON object (``plainJSON(_:)``). An unknown auth_mode makes the file unreadable to
    /// Codex itself, so it is codex-error.
    ///
    /// Every check and lookup reads `raw` where it lies (``WireJSON/Borrowed``): member names match as decoded bytes,
    /// as in Go and Codex (a Swift `String` key would take "OPENAI_API_\u{212A}EY", with a Kelvin sign, for
    /// OPENAI_API_KEY), the mode is compared in place with the modes Codex knows, and a value of the wrong kind is
    /// refused from its first byte. The access token, decoded straight into the `String` kept, is the one copy made
    /// of anything in the file.
    static func decode(borrowing raw: UnsafeRawBufferPointer) throws -> Token? {
        guard let doc = plainJSON(raw), doc.kind(doc.root) == UInt8(ascii: "{") else { return nil }
        let root = doc.root
        func present(_ key: String) -> Bool { doc.member(root, key).map { !doc.isNull($0) } ?? false }
        var mode = modeChatGPT
        if let m = doc.member(root, "auth_mode"), !doc.isNull(m) {
            guard let known = ([modeChatGPT, modeAPIKey] + fileModes).first(where: { doc.stringEquals(m, $0) }) else {
                throw CodexAuthError(.codexError, message: "auth.json names an unknown auth_mode")
            }
            mode = known
        } else if present("personal_access_token") {
            mode = "personalAccessToken"
        } else if present("bedrock_api_key") {
            mode = "bedrockApiKey"
        } else if present("bedrock_access_keys") {
            mode = "bedrockAccessKeys"
        } else if present("OPENAI_API_KEY") {
            mode = modeAPIKey
        }
        if let e = classifyMode(mode) { throw e }
        // A tokens value that is not an object holds no access token.
        guard let tokens = doc.member(root, "tokens"), let a = doc.member(tokens, "access_token"),
            let access = doc.string(a), !access.isEmpty
        else { throw CodexAuthError(.codexError, message: "auth.json has a ChatGPT sign-in without an access token") }
        return makeToken(access, source: "file")
    }
}

/// Registers `bearer` with the scrubber and wraps it with its expiry and account: the JWT `exp` claim, read without
/// checking the signature, and the account the token names (``accountClaim(_:)``). A token that is not a JWT, or has no
/// `exp` that is a signed 64-bit integer, has unknown expiry.
func makeToken(_ bearer: String, source: String) -> Token {
    let claims = jwtClaims(bearer)
    return Token(bearer: bearer, expires: claims.expires, source: source, accountID: claims.account)
}

/// ``jwtExpiry(_:)`` and ``accountClaim(_:)`` read from one decoding of the payload.
func jwtClaims(_ token: String) -> (expires: Date?, account: String?) {
    withJWTPayload(token) { doc in (expiryClaim(doc), accountClaim(doc)) } ?? (nil, nil)
}

/// The `exp` claim, read in place from the payload decoded into a buffer wiped before it is freed: the token is
/// kept, but no other copy of what it holds is.
func jwtExpiry(_ token: String) -> Date? { withJWTPayload(token, expiryClaim) }

private func expiryClaim(_ doc: WireJSON.Borrowed) -> Date? {
    // A signed 64-bit integer exactly, as Go decodes it: not a fraction, an exponent or a wrapped value.
    guard let exp = doc.member(doc.root, "exp"), let v = doc.int64(exp) else { return nil }
    return Date(timeIntervalSince1970: TimeInterval(v))
}

/// The longest account ID a private sign-in names.
let maxAccountID = 256

/// The ChatGPT account `token` names: `chatgpt_account_id` in its `https://api.openai.com/auth` claim
/// (spec/credentials.md, AUTH-08). nil when `token` is not a JWT, or the claim is absent or not a string of 1 to 256
/// ASCII letters, digits, `-`, `_` and `.`. Names are exact, as for `exp`.
func accountClaim(_ token: String) -> String? { withJWTPayload(token, accountClaim) }

private func accountClaim(_ doc: WireJSON.Borrowed) -> String? {
    guard let auth = doc.member(doc.root, "https://api.openai.com/auth"),
        let v = doc.member(auth, "chatgpt_account_id"),
        let id = doc.string(v, atMost: maxAccountID), !id.isEmpty,
        id.utf8.allSatisfy({ base64URL($0) != nil || $0 == UInt8(ascii: ".") })
    else { return nil }
    return id
}

/// Decodes `token`'s payload into a buffer wiped before it is freed and, when the payload is JSON as Codex's is read,
/// passes it to `body`, which must not keep it; nil when `token` is not a JWT or the payload is not such JSON.
func withJWTPayload<R>(_ token: String, _ body: (WireJSON.Borrowed) -> R?) -> R? {
    let u = token.utf8
    guard let first = u.firstIndex(of: 0x2E) else { return nil }
    let start = u.index(after: first)
    guard let second = u[start...].firstIndex(of: 0x2E), !u[u.index(after: second)...].contains(0x2E) else {
        return nil
    }
    let payload = u[start..<second]
    // The base64url alphabet only, unpadded (RFC 7515, and Codex's own decoder): "eyJleHAiOjF9====" is no JWT.
    var count = 0
    for c in payload {
        guard base64URL(c) != nil else { return nil }
        count += 1
    }
    guard count % 4 != 1 else { return nil }
    var bytes = [UInt8](repeating: 0, count: count * 3 / 4)
    defer { bytes.withUnsafeMutableBytes { _ = memset_s($0.baseAddress, $0.count, 0, $0.count) } }
    var acc: UInt32 = 0, bits = 0, k = 0
    for c in payload {
        acc = acc << 6 | base64URL(c)!
        bits += 6
        if bits >= 8 {
            bits -= 8
            bytes[k] = UInt8(truncatingIfNeeded: acc >> bits)
            k += 1
        }
        acc &= (1 << bits) - 1
    }
    // The bits left over are ignored, as Foundation's and Go's decoders ignore them.
    return bytes.withUnsafeBytes { raw -> R? in
        guard let doc = plainJSON(raw) else { return nil }
        return body(doc)
    }
}

/// A base64url digit's value.
func base64URL(_ c: UInt8) -> UInt32? {
    switch c {
    case 0x41...0x5A: return UInt32(c - 0x41)
    case 0x61...0x7A: return UInt32(c - 0x61 + 26)
    case 0x30...0x39: return UInt32(c - 0x30 + 52)
    case UInt8(ascii: "-"): return 62
    case UInt8(ascii: "_"): return 63
    default: return nil
    }
}
#endif
