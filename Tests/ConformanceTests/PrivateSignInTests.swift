#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import RealtimeConnection
import XCTest

/// A private sign-in fixture (AUTH-08), mirroring go/conformance/private_test.go: the token is written into the
/// folder the case sets up, and every expectation key is checked against what the folder holds afterwards.
enum PrivateSignInCase {
    static func run(_ s: [String: Any], now: Int64, want: [String: Any]) throws -> [String] {
        guard let bearer = Fixtures.substitute(s["token"] as Any) as? String else {
            return ["setup.privateSignIn.token is not a string or a $jwt"]
        }
        let root = try temporaryFolder("sayso-private-")
        defer { try? FileManager.default.removeItem(atPath: root) }
        let (arg, listed) = try folder(root, s["folder"] as? String ?? "")
        var error: Error?
        do {
            try writePrivateSignIn(
                Token(bearer: bearer, expires: nil), codexHome: arg, now: Date(timeIntervalSince1970: TimeInterval(now)))
        } catch let e {
            error = e
        }
        let description = error.map(String.init(describing:)) ?? ""
        var p: [String] = []
        for (key, v) in want {
            switch key {
            case "state":
                let got = CodexAuthError.state(of: error).rawValue
                if got != v as? String { p.append("state \(got), want \(v)") }
            case "class":
                let got = ErrorClass.of(error)?.rawValue ?? ""
                if got != v as? String { p.append("class \(got.debugDescription), want \(v)") }
            case "signIn":
                let want = (v as? String ?? "").replacingOccurrences(of: "$token", with: bearer)
                switch regularContent(listed + "/auth.json") {
                case .success(let got): if got != want { p.append("signIn differs") }  // never printed: it holds the token
                case .failure(let e): p.append("signIn: \(e)")
                }
            case "fileMode":
                var st = stat()
                if lstat(listed + "/auth.json", &st) != 0 {
                    p.append("fileMode: no auth.json")
                } else if String(format: "%04o", st.st_mode & 0o7777) != v as? String {
                    p.append("fileMode \(String(format: "%04o", st.st_mode & 0o7777)), want \(v)")
                }
            case "entries":
                let got = entries(listed)
                if (got as NSDictionary?) != (v as? NSDictionary) { p.append("entries \(got ?? [:]), want \(v)") }
            case "elsewhere":
                if case .success(let got) = regularContent(root + "/elsewhere"), got == v as? String { break }
                p.append("elsewhere differs, want \(v)")
            case "notInDescription":
                for s in v as? [String] ?? [] where description.contains(s) { p.append("\(s.debugDescription) leaked") }
            default:
                p.append("unknown expectation \(key)")
            }
        }
        if let error, !(error is CodexAuthError) { p.append("the error is not a CodexAuthError") }
        return p
    }

    /// A new folder in the temporary folder, its path resolved.
    static func temporaryFolder(_ prefix: String) throws -> String {
        guard let tmp = realpath(NSTemporaryDirectory(), nil) else { throw FixtureError("no temporary folder") }
        defer { free(tmp) }
        let root = String(cString: tmp) + "/" + prefix + UUID().uuidString
        try FileManager.default.createDirectory(atPath: root, withIntermediateDirectories: false)
        return root
    }

    /// Sets up the folder a case names under `root`: the path passed to writePrivateSignIn, and the folder whose
    /// entries are compared afterwards (the one a link names).
    static func folder(_ root: String, _ kind: String) throws -> (arg: String, listed: String) {
        let arg = root + "/codex"
        func mk(_ mode: mode_t) throws {
            guard mkdir(arg, 0o700) == 0, chmod(arg, mode) == 0 else { throw FixtureError("cannot make the folder") }
        }
        func file(_ path: String) throws {
            try Data("{}".utf8).write(to: URL(fileURLWithPath: path))
            chmod(path, 0o600)
        }
        switch kind {
        case "private": try mk(0o700)
        case "open": try mk(0o755)
        case "searchable": try mk(0o711)
        case "link":
            let real = root + "/real"
            guard mkdir(real, 0o700) == 0, symlink(real, arg) == 0 else { throw FixtureError("cannot make the link") }
            return (arg, real)
        case "missing": break
        case "file": try file(arg)
        case "relative": return ("relative/codex", arg)
        case "taken":
            try mk(0o700)
            try file(arg + "/auth.json")
        case "taken-link":
            try mk(0o700)
            try file(root + "/elsewhere")
            guard symlink(root + "/elsewhere", arg + "/auth.json") == 0 else { throw FixtureError("cannot make the link") }
        default: throw FixtureError("unknown folder \(kind)")
        }
        return (arg, arg)
    }

    /// The content of the regular file at `path`, read without following a link.
    static func regularContent(_ path: String) -> Result<String, FixtureError> {
        var st = stat()
        guard lstat(path, &st) == 0 else { return .failure(FixtureError("missing")) }
        guard st.st_mode & S_IFMT == S_IFREG else { return .failure(FixtureError("not a regular file")) }
        guard let data = FileManager.default.contents(atPath: path) else { return .failure(FixtureError("unreadable")) }
        return .success(String(decoding: data, as: UTF8.self))
    }

    /// Each entry of `dir` with its kind: file, link, folder or other; nil when `dir` cannot be listed.
    static func entries(_ dir: String) -> [String: String]? {
        guard let names = try? FileManager.default.contentsOfDirectory(atPath: dir) else { return nil }
        var out: [String: String] = [:]
        for name in names {
            var st = stat()
            guard lstat(dir + "/" + name, &st) == 0 else { continue }
            switch st.st_mode & S_IFMT {
            case S_IFREG: out[name] = "file"
            case S_IFLNK: out[name] = "link"
            case S_IFDIR: out[name] = "folder"
            default: out[name] = "other"
            }
        }
        return out
    }
}

/// The fixtures (fixtures/private-sign-in/) hold the contract; these tests cover what a fixture cannot set up, as in
/// go/codexauth/private_test.go: the umask, a write cut short, a path holding a NUL, and what the write leaves in
/// memory.
final class PrivateSignInTests: XCTestCase {
    /// {"exp":1800003600,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}
    private static let payload =
        "eyJleHAiOjE4MDAwMDM2MDAsImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LTEifX0"
    private static let token = Token(bearer: "eyJhbGciOiJub25lIn0." + payload + ".c2ln", expires: nil)
    private static let now = Date(timeIntervalSince1970: 1_800_000_000)

    /// A folder that grants nothing to anyone else, as AUTH-08 requires.
    private func privateHome() throws -> String {
        let root = try PrivateSignInCase.temporaryFolder("sayso-private-")
        addTeardownBlock { try? FileManager.default.removeItem(atPath: root) }
        XCTAssertEqual(mkdir(root + "/codex", 0o700), 0)
        XCTAssertEqual(chmod(root + "/codex", 0o700), 0)
        return root + "/codex"
    }

    /// The file is 0600 whatever the umask: one that removes the owner's write bit, or every bit, still leaves a file
    /// the private Codex can read, and one that keeps group and other bits leaves none for them.
    func testAPrivateSignInIsPrivateWhateverTheUmask() throws {
        for mask: mode_t in [0, 0o022, 0o277, 0o777] {
            let home = try privateHome()
            let old = umask(mask)
            defer { umask(old) }
            try writePrivateSignIn(Self.token, codexHome: home, now: Self.now)
            var st = stat()
            XCTAssertEqual(lstat(home + "/auth.json", &st), 0)
            XCTAssertEqual(st.st_mode & 0o7777, 0o600, "umask \(String(mask, radix: 8))")
        }
    }

    /// A write cut short (here by the file size limit) removes the file it made: a private Codex never starts from a
    /// partial sign-in.
    func testAWriteCutShortLeavesNoFile() throws {
        let home = try privateHome()
        let oldHandler = signal(SIGXFSZ, SIG_IGN)  // the write then fails with EFBIG instead of ending the process
        defer { signal(SIGXFSZ, oldHandler) }
        var old = rlimit()
        XCTAssertEqual(getrlimit(RLIMIT_FSIZE, &old), 0)
        var limit = rlimit(rlim_cur: 16, rlim_max: old.rlim_max)
        XCTAssertEqual(setrlimit(RLIMIT_FSIZE, &limit), 0)
        var state = CodexAuthState.ready
        do { try writePrivateSignIn(Self.token, codexHome: home, now: Self.now) } catch let e as CodexAuthError {
            state = e.state
        }
        XCTAssertEqual(setrlimit(RLIMIT_FSIZE, &old), 0)
        XCTAssertEqual(state, .codexError)
        var st = stat()
        XCTAssertNotEqual(lstat(home + "/auth.json", &st), 0, "a partial auth.json was left behind")
    }

    func testAPrivateHomePathHoldingANULIsRefused() throws {
        let home = try privateHome()
        XCTAssertThrowsError(try writePrivateSignIn(Self.token, codexHome: home + "\u{0}/x", now: Self.now)) {
            XCTAssertEqual(($0 as? CodexAuthError)?.state, .codexError)
        }
        var st = stat()
        XCTAssertNotEqual(lstat(home + "/auth.json", &st), 0)
    }

    /// Writing a private sign-in leaves no copy of the token in memory beyond the one kept, and no byte of its
    /// payload: the file's bytes are built in memory wiped once written, and the payload is decoded into a buffer
    /// wiped once read, on success and when the write is refused.
    func testAPrivateSignInKeepsOnlyTheToken() throws {
        if MemoryScan.allocatorReplaced { throw XCTSkip("a sanitizer replaced the allocator the scan watches") }
        for taken in [false, true] {
            let scan = MemoryScan()
            let sig = scan.needle()  // the token's signature: every copy of the token holds it
            let start = scan.mark
            scan.append(#"{"exp":1800003600,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"},"x":""#)
            scan.append(copyOf: scan.needle())
            scan.append(#""}"#)
            let encoded = base64url(scan.bytes(start..<scan.mark))  // holds no needle
            scan.reset(to: start)
            // One string holds the token, built from memory wiped before it is freed.
            let token: Token = {
                var raw = Array("eyJhbGciOiJub25lIn0.".utf8) + encoded + [UInt8(ascii: ".")]
                raw.append(contentsOf: scan.bytes(sig))
                defer { raw.withUnsafeMutableBytes { _ = memset_s($0.baseAddress, $0.count, 0, $0.count) } }
                return Token(bearer: String(decoding: raw, as: UTF8.self), expires: nil)
            }()
            let home = try privateHome()
            if taken { try Data("{}".utf8).write(to: URL(fileURLWithPath: home + "/auth.json")) }
            let before = scan.occurrences()
            XCTAssertEqual(before, [1, 0], "the test itself left a copy")
            var failed = false
            let freed = scan.frees {
                do { try writePrivateSignIn(token, codexHome: home, now: Self.now) } catch { failed = true }
            }
            XCTAssertEqual(failed, taken)
            XCTAssertEqual(freed, [0, 0], "taken \(taken): a copy was freed unwiped")
            XCTAssertEqual(scan.occurrences(), before, "taken \(taken): a copy outlived the write")
            withExtendedLifetime(token) {}
        }
    }

    /// Base64url without padding; the result holds the encoding's bytes, which hold no needle.
    private func base64url(_ b: UnsafeRawBufferPointer) -> [UInt8] {
        let alphabet = Array("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_".utf8)
        var out: [UInt8] = []
        var i = 0
        while i < b.count {
            let n = min(3, b.count - i)
            var v: UInt32 = 0
            for k in 0..<3 { v = v << 8 | (k < n ? UInt32(b[i + k]) : 0) }
            for k in 0..<(n + 1) { out.append(alphabet[Int((v >> (18 - 6 * k)) & 63)]) }
            i += 3
        }
        return out
    }
}
#endif
