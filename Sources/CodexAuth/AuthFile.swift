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
        var confined = false
        if let confine, !confine.isEmpty {
            guard let root = realPath(confine) else {
                throw CodexAuthError(.codexError, message: "the folder CODEX_HOME is confined to does not resolve")
            }
            guard real == root || real.utf8.starts(with: (root == "/" ? "/" : root + "/").utf8) else {
                throw CodexAuthError(.codexError, message: "CODEX_HOME is outside the folder it is confined to")
            }
            confined = true
        }
        // The resolved path holds no link, so it is opened from "/" one folder at a time, never following one: a
        // folder of the path replaced since it was resolved (by a link elsewhere, say) fails the open instead of
        // redirecting it, and the folder opened is the one the confinement above was checked on.
        guard let dir = openResolved(real) else {
            let what = confined ? "CODEX_HOME is not a folder inside the folder it is confined to" : "CODEX_HOME is not a folder"
            throw CodexAuthError(.codexError, message: what)
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

    /// The folder at `path`, an absolute path holding no link, opened from "/" one component at a time without
    /// following a link and without blocking on a FIFO, each component checked to be a folder on its own descriptor;
    /// nil on failure. Each folder is opened for search only (O_SEARCH): walking a path needs search permission on
    /// its folders, not read permission, so an ancestor such as a 0711 home folder does not refuse it.
    static func openResolved(_ path: String) -> Int32? {
        let flags = O_SEARCH | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC
        guard path.utf8.first == 0x2F, !path.utf8.contains(0) else { return nil }
        var fd = openRetrying { open("/", flags) }
        guard fd >= 0 else { return nil }
        for part in path.utf8.split(separator: 0x2F).map({ String(decoding: $0, as: UTF8.self) }) {
            guard part != ".", part != ".." else {  // a resolved path has neither; refused rather than walked
                Darwin.close(fd)
                return nil
            }
            let next = openRetrying { openat(fd, part, flags) }
            Darwin.close(fd)
            guard next >= 0 else { return nil }
            var st = stat()
            guard fstat(next, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else {
                Darwin.close(next)
                return nil
            }
            fd = next
        }
        return fd
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

    private static func realPath(_ p: String) -> String? {
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
        var data = Data(buf[0..<used])
        defer { data.resetBytes(in: 0..<data.count) }
        guard isPlainJSON(data) else { return nil }
        return try decode(data)
    }

    /// The auth_mode values Codex knows (protocol/src/auth.rs), other than chatgpt and apikey. Compared as bytes.
    private static let fileModes = [
        "chatgptAuthTokens", "headers", "agentIdentity", "personalAccessToken", "bedrockApiKey", "bedrockAccessKeys",
    ]

    /// Resolves the sign-in mode in Codex's own order (AuthDotJson::resolved_mode) and returns the ChatGPT token; nil
    /// when `data`, already validated as plain JSON, is not one object. An unknown auth_mode makes the file
    /// unreadable to Codex itself, so it is codex-error.
    ///
    /// Only the members needed are read, as the bytes the file holds, and member names match as bytes, as in Go and
    /// Codex (a Swift `String` key would take "OPENAI_API_\u{212A}EY", with a Kelvin sign, for OPENAI_API_KEY). No
    /// other value, such as a refresh token or an API key, ever becomes a `String`, which could not be wiped; each
    /// copy of a member's bytes is wiped once read. Only the mode and the access token are decoded.
    static func decode(_ data: Data) throws -> Token? {
        do { _ = try WireJSON.memberBytes(data, "auth_mode") } catch { return nil }  // not an object
        func present(_ key: String) throws -> Bool { try withMember(data, key) { $0.map { !isNull($0) } ?? false } }
        var mode = modeChatGPT
        if try present("auth_mode") {
            let m = try withMember(data, "auth_mode") { $0.flatMap(stringValue) }
            guard let m, let known = ([modeChatGPT, modeAPIKey] + fileModes).first(where: { $0.utf8.elementsEqual(m.utf8) })
            else { throw CodexAuthError(.codexError, message: "auth.json names an unknown auth_mode") }
            mode = known
        } else if try present("personal_access_token") {
            mode = "personalAccessToken"
        } else if try present("bedrock_api_key") {
            mode = "bedrockApiKey"
        } else if try present("bedrock_access_keys") {
            mode = "bedrockAccessKeys"
        } else if try present("OPENAI_API_KEY") {
            mode = modeAPIKey
        }
        if let e = classifyMode(mode) { throw e }
        let access = try withMember(data, "tokens") { tokens -> String? in
            guard let tokens, !isNull(tokens) else { return nil }
            // A tokens value that is not an object holds no access token.
            return (try? withMember(tokens, "access_token") { $0.flatMap(stringValue) }) ?? nil
        }
        guard let access, !access.isEmpty else {
            throw CodexAuthError(.codexError, message: "auth.json has a ChatGPT sign-in without an access token")
        }
        return makeToken(access, source: "file")
    }

    /// The bytes of the member `name` of the object `data`, nil when absent, handed to `body` and wiped after it.
    private static func withMember<T>(_ data: Data, _ name: String, _ body: (Data?) throws -> T) throws -> T {
        var raw = try WireJSON.memberBytes(data, name)
        defer { if let n = raw?.count { raw?.resetBytes(in: 0..<n) } }
        return try body(raw)
    }

    private static func isNull(_ raw: Data) -> Bool { raw.elementsEqual("null".utf8) }

    /// A JSON string's value; nil for any other value.
    private static func stringValue(_ raw: Data) -> String? {
        (try? JSONSerialization.jsonObject(with: raw, options: [.fragmentsAllowed])) as? String
    }
}

/// Registers `bearer` with the scrubber and wraps it with its expiry: the JWT `exp` claim, read without checking the
/// signature. A token that is not a JWT, or has no `exp` that is a signed 64-bit integer, has unknown expiry.
func makeToken(_ bearer: String, source: String) -> Token {
    Token(bearer: bearer, expires: jwtExpiry(bearer), source: source)
}

func jwtExpiry(_ token: String) -> Date? {
    let parts = token.split(separator: ".", omittingEmptySubsequences: false)
    guard parts.count == 3 else { return nil }
    // The base64url alphabet only, unpadded (RFC 7515, and Codex's own decoder): "eyJleHAiOjF9====" is no JWT.
    let payload = String(parts[1])
    guard payload.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }) else {
        return nil
    }
    var b64 = payload.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
    guard b64.count % 4 != 1 else { return nil }
    b64 += String(repeating: "=", count: (4 - b64.count % 4) % 4)
    guard let data = Data(base64Encoded: b64), isPlainJSON(data),
        let claims = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
        let exp = claims["exp"] as? NSNumber, CFGetTypeID(exp) != CFBooleanGetTypeID(), !CFNumberIsFloatType(exp),
        NSNumber(value: exp.int64Value) == exp  // a signed 64-bit integer exactly, as Go decodes it; not wrapped
    else { return nil }
    return Date(timeIntervalSince1970: TimeInterval(exp.int64Value))
}
#endif
