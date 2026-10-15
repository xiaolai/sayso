import Foundation

/// The process-wide scrubber (spec/credentials.md, hygiene). It removes every registered secret and
/// anything shaped like a bearer JWT or an `ek_` client secret, whatever its source.
public enum Secret {
    /// What every removed secret becomes.
    public static let redacted = "<redacted>"

    private static let registry = Registry()

    /// Adds `secret` to the scrub list. Empty strings are ignored. The list only grows: a secret seen once stays secret.
    public static func register(_ secret: String) {
        guard !secret.isEmpty else { return }
        registry.add(secret)
    }

    /// `text` with every registered secret and every secret-shaped pattern replaced by ``redacted``. Use it on any text
    /// that may reach a user, a log or an artifact. Matching is on the UTF-8 bytes, so a combining mark after a secret
    /// cannot hide it, and every match is found in the original text: the union of the matched spans is replaced, so a
    /// registered secret inside a longer secret-shaped value never leaves the rest of that value behind.
    public static func scrub(_ text: String) -> String {
        let bytes = Array(text.utf8)
        var spans: [Range<Int>] = []
        for secret in registry.all() { spans += occurrences(of: secret, in: bytes) }
        spans += patternSpans(bytes)
        guard !spans.isEmpty else { return text }
        spans.sort { $0.lowerBound < $1.lowerBound }
        var out: [UInt8] = []
        out.reserveCapacity(bytes.count)
        var at = 0
        var i = 0
        while i < spans.count {
            var span = spans[i]
            i += 1
            while i < spans.count, spans[i].lowerBound <= span.upperBound {  // overlapping or touching spans merge
                span = span.lowerBound..<max(span.upperBound, spans[i].upperBound)
                i += 1
            }
            out += bytes[at..<span.lowerBound]
            out += redacted.utf8
            at = span.upperBound
        }
        out += bytes[at...]
        return String(decoding: out, as: UTF8.self)
    }

    /// Every occurrence of `needle` in `haystack`, overlapping ones included, in O(n + m) (Knuth-Morris-Pratt), as in
    /// Go: a naive scan is quadratic for a long, repetitive secret against a long, repetitive text.
    static func occurrences(of needle: [UInt8], in haystack: [UInt8]) -> [Range<Int>] {
        let m = needle.count
        guard m > 0, m <= haystack.count else { return [] }
        var fail = [Int](repeating: 0, count: m)  // fail[q]: the longest proper border of needle[0...q]
        var k = 0
        for q in 1..<m {
            while k > 0, needle[q] != needle[k] { k = fail[k - 1] }
            if needle[q] == needle[k] { k += 1 }
            fail[q] = k
        }
        var found: [Range<Int>] = []
        k = 0
        for (i, b) in haystack.enumerated() {
            while k > 0, b != needle[k] { k = fail[k - 1] }
            if b == needle[k] { k += 1 }
            if k == m {
                found.append(i + 1 - m..<i + 1)
                k = fail[k - 1]
            }
        }
        return found
    }

    /// The matches of `eyJ[\w-]+\.[\w-]+\.[\w-]+|ek_[\w-]+` (`\w` is ASCII `[A-Za-z0-9_]`, as in Go's RE2) starting at
    /// every position, overlapping ones included: a JWT can start inside an `ek_` match (`ek_eyJa.b.c`) and reach past
    /// it. `[\w-]` excludes `.`, so each run is maximal and a match is decided in constant time from the precomputed
    /// run ends: the scan is linear.
    static func patternSpans(_ bytes: [UInt8]) -> [Range<Int>] {
        func isWord(_ b: UInt8) -> Bool {
            (b >= 0x30 && b <= 0x39) || (b >= 0x41 && b <= 0x5A) || (b >= 0x61 && b <= 0x7A) || b == 0x5F || b == 0x2D
        }
        let n = bytes.count
        var runEnd = [Int](repeating: 0, count: n + 1)  // runEnd[i]: the end of the word run starting at i
        runEnd[n] = n
        for i in stride(from: n - 1, through: 0, by: -1) { runEnd[i] = isWord(bytes[i]) ? runEnd[i + 1] : i }
        func has(_ a: UInt8, _ b: UInt8, _ c: UInt8, at i: Int) -> Bool {
            i + 3 <= n && bytes[i] == a && bytes[i + 1] == b && bytes[i + 2] == c
        }
        var spans: [Range<Int>] = []
        var i = 0
        while i < n {
            if has(0x65, 0x79, 0x4A, at: i) {  // eyJ, then three dot-separated non-empty runs
                let a = runEnd[i + 3]
                if a > i + 3, a < n, bytes[a] == 0x2E {
                    let b = runEnd[a + 1]
                    if b > a + 1, b < n, bytes[b] == 0x2E {
                        let c = runEnd[b + 1]
                        if c > b + 1 { spans.append(i..<c) }
                    }
                }
            }
            if has(0x65, 0x6B, 0x5F, at: i) {  // ek_
                let e = runEnd[i + 3]
                if e > i + 3 { spans.append(i..<e) }
            }
            i += 1
        }
        return spans
    }

    /// The registered secrets as UTF-8 bytes. Bytes, not `String`s: `String` equality is canonical equivalence, so a
    /// secret spelled with a combining mark would count as already registered when its precomposed twin was.
    private final class Registry: @unchecked Sendable {
        private let lock = NSLock()
        private var known: Set<[UInt8]> = []
        private var list: [[UInt8]] = []

        func add(_ s: String) {
            let bytes = Array(s.utf8)
            lock.lock()
            defer { lock.unlock() }
            if known.insert(bytes).inserted { list.append(bytes) }
        }

        func all() -> [[UInt8]] {
            lock.lock()
            defer { lock.unlock() }
            return list
        }
    }
}

/// A sign-in bearer with its metadata. Every formatting and encoding path prints ``Secret/redacted``; the bearer is
/// reachable only through ``bearer``, to build an `Authorization` header.
public struct Token: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable,
    Encodable
{
    public let bearer: String
    /// The JWT expiry; nil when unknown (a token that is not a JWT, or has no `exp`).
    public let expires: Date?
    public let plan: String
    public let source: String

    /// Registers `bearer` with the scrubber.
    public init(bearer: String, expires: Date?, plan: String = "", source: String = "") {
        Secret.register(bearer)
        self.bearer = bearer
        self.expires = expires
        self.plan = plan
        self.source = source
    }

    public var description: String { Secret.redacted }
    public var debugDescription: String { Secret.redacted }
    public var customMirror: Mirror { Mirror(self, children: ["bearer": Secret.redacted], displayStyle: .struct) }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        try c.encode(Secret.redacted)
    }

    /// Byte equality, as in Go: `String` equality is canonical equivalence, which would make two different bearers
    /// equal when they differ only in how an accented letter is encoded.
    public static func == (a: Token, b: Token) -> Bool {
        a.bearer.utf8.elementsEqual(b.bearer.utf8) && a.expires == b.expires && a.plan.utf8.elementsEqual(b.plan.utf8)
            && a.source.utf8.elementsEqual(b.source.utf8)
    }
}

/// A minted ephemeral `ek_` realtime client secret, redacted like ``Token``.
public struct ClientSecret: Sendable, Equatable, CustomStringConvertible, CustomDebugStringConvertible,
    CustomReflectable, Encodable
{
    public let bearer: String
    public let expiresAt: Date

    /// Registers `bearer` with the scrubber.
    public init(bearer: String, expiresAt: Date) {
        Secret.register(bearer)
        self.bearer = bearer
        self.expiresAt = expiresAt
    }

    public var description: String { Secret.redacted }
    public var debugDescription: String { Secret.redacted }
    public var customMirror: Mirror { Mirror(self, children: ["bearer": Secret.redacted], displayStyle: .struct) }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        try c.encode(Secret.redacted)
    }

    /// Byte equality, as for ``Token``.
    public static func == (a: ClientSecret, b: ClientSecret) -> Bool {
        a.bearer.utf8.elementsEqual(b.bearer.utf8) && a.expiresAt == b.expiresAt
    }
}

/// Yields the sign-in bearer and refreshes it after a refusal. CodexAuth's `Credentials` implements it; a test or an
/// app with another sign-in can too.
public protocol CredentialSource: Sendable {
    func token() async throws -> Token
    /// A fresh token after `stale` was refused. When another caller already replaced `stale`, returns the
    /// replacement without a second refresh.
    func refresh(after stale: Token) async throws -> Token
}

/// Yields a client secret for a session object. Sessions take a SecretSource, never a sign-in, so a secret obtained
/// elsewhere changes no API. ``Minter`` implements it.
public protocol SecretSource: Sendable {
    func secret(session: Data) async throws -> ClientSecret
}
