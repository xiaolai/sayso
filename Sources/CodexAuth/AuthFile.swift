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
    /// inside `confine` when that is set; `~/.codex/auth.json` when `codexHome` is unset. A set CODEX_HOME that
    /// fails is codex-error.
    static func location(codexHome: String?, home: String, confine: String?) throws -> AuthLocation {
        guard let codexHome, !codexHome.isEmpty else { return AuthLocation(dir: -1, path: join(home, ".codex/auth.json")) }
        guard !codexHome.utf8.contains(0), !(confine ?? "").utf8.contains(0) else {
            throw CodexAuthError(.codexError, message: "CODEX_HOME is not a valid path")
        }
        guard codexHome.utf8.first == 0x2F else { throw CodexAuthError(.codexError, message: "CODEX_HOME is not an absolute path") }
        guard let real = realPath(codexHome) else {
            throw CodexAuthError(.codexError, message: "CODEX_HOME does not resolve")
        }
        let dir: Int32
        if let confine, !confine.isEmpty {
            guard let root = realPath(confine) else {
                throw CodexAuthError(.codexError, message: "the folder CODEX_HOME is confined to does not resolve")
            }
            guard real == root || real.utf8.starts(with: (root == "/" ? "/" : root + "/").utf8) else {
                throw CodexAuthError(.codexError, message: "CODEX_HOME is outside the folder it is confined to")
            }
            // Opened from the confining folder one component at a time, never following a link, so a path changed
            // since it was resolved cannot leave the folder.
            guard let d = openBeneath(root, String(decoding: real.utf8.dropFirst(root.utf8.count), as: UTF8.self)) else {
                throw CodexAuthError(
                    .codexError, message: "CODEX_HOME is not a folder inside the folder it is confined to")
            }
            dir = d
        } else {
            dir = open(real, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
            guard dir >= 0 else { throw CodexAuthError(.codexError, message: "CODEX_HOME is not a folder") }
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

    /// `rel` (folders separated by "/") opened beneath the folder `root`, without following a link; nil on failure.
    private static func openBeneath(_ root: String, _ rel: String) -> Int32? {
        var fd = open(root, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        guard fd >= 0 else { return nil }
        for part in rel.utf8.split(separator: 0x2F).map({ String(decoding: $0, as: UTF8.self) }) {
            let next = openat(fd, part, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            Darwin.close(fd)
            guard next >= 0 else { return nil }
            fd = next
        }
        return fd
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
        let fd = loc.dir >= 0 ? openat(loc.dir, fileName, flags) : open(loc.path, flags)
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
        guard isPlainJSON(data),
            let doc = (try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])) as? [String: Any]
        else { return nil }
        return try decode(doc)
    }

    /// The auth_mode values Codex knows (protocol/src/auth.rs), other than chatgpt and apikey.
    private static let fileModes: Set<String> = [
        "chatgptAuthTokens", "headers", "agentIdentity", "personalAccessToken", "bedrockApiKey", "bedrockAccessKeys",
    ]

    /// Resolves the sign-in mode in Codex's own order (AuthDotJson::resolved_mode) and returns the ChatGPT token. An
    /// unknown auth_mode makes the file unreadable to Codex itself, so it is codex-error.
    static func decode(_ doc: [String: Any]) throws -> Token {
        func present(_ key: String) -> Bool { doc[key].map { !($0 is NSNull) } ?? false }
        var mode = modeChatGPT
        if present("auth_mode") {
            guard let m = doc["auth_mode"] as? String, m == modeChatGPT || m == modeAPIKey || fileModes.contains(m) else {
                throw CodexAuthError(.codexError, message: "auth.json names an unknown auth_mode")
            }
            mode = m
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
        guard let tokens = doc["tokens"] as? [String: Any], let access = tokens["access_token"] as? String,
            !access.isEmpty
        else { throw CodexAuthError(.codexError, message: "auth.json has a ChatGPT sign-in without an access token") }
        return makeToken(access, source: "file")
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
    var payload = String(parts[1])
    while payload.hasSuffix("=") { payload.removeLast() }
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
