import Foundation

/// The process-wide scrubber (spec/credentials.md, hygiene). It removes every registered secret and
/// anything shaped like a bearer JWT or an `ek_` client secret, whatever its source.
public enum Secret {
    /// What every removed secret becomes.
    public static let redacted = "<redacted>"

    static let registry = Registry()

    /// `date` when it is finite; otherwise a definite date: NaN and minus infinity are `Date.distantPast` (an expiry
    /// that has long passed, never a moment that is both expired and not), plus infinity is `Date.distantFuture`.
    static func definite(_ date: Date) -> Date {
        let t = date.timeIntervalSince1970
        if t.isFinite { return date }
        return t == .infinity ? .distantFuture : .distantPast
    }

    /// Adds `secret` to the scrub list. Empty strings are ignored. A secret seen once stays secret, and nothing of it
    /// is kept: its UTF-8 bytes are read in place for their length and two hashes (see `Registry`).
    public static func register(_ secret: String) {
        guard !secret.isEmpty else { return }
        registry.add(secret)
    }

    /// `text` with every registered secret and every secret-shaped pattern replaced by ``redacted``. Use it on any text
    /// that may reach a user, a log or an artifact. Matching is on the UTF-8 bytes, so a combining mark after a secret
    /// cannot hide it, and every match is found in the original text: the union of the matched spans is replaced, so a
    /// registered secret inside a longer secret-shaped value never leaves the rest of that value behind.
    public static func scrub(_ text: String) -> String {
        var text = text
        return text.withUTF8 { scrubbed(UnsafeRawBufferPointer($0)) } ?? text
    }

    /// The UTF-8 text `utf8` scrubbed as ``scrub(_:)`` scrubs a `String`, read where it lies: only the scrubbed text is
    /// made a `String`, so a secret it removes leaves no copy behind.
    package static func scrub(utf8: UnsafeRawBufferPointer) -> String {
        scrubbed(utf8) ?? String(decoding: utf8, as: UTF8.self)
    }

    /// `bytes` with every match replaced by ``redacted``; nil when nothing matches. The bytes are read in place: the
    /// scrubber makes no copy of the text it is given, which may hold a secret.
    private static func scrubbed(_ bytes: UnsafeRawBufferPointer) -> String? {
        var spans = registry.spans(in: bytes)
        spans += patternSpans(bytes)
        guard !spans.isEmpty else { return nil }
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

    /// The matches of `eyJ[\w-]+\.[\w-]+\.[\w-]+|ek_[\w-]+` (`\w` is ASCII `[A-Za-z0-9_]`, as in Go's RE2) starting at
    /// every position, overlapping ones included: a JWT can start inside an `ek_` match (`ek_eyJa.b.c`) and reach past
    /// it. `[\w-]` excludes `.`, so each run is maximal and a match is decided in constant time from the precomputed
    /// run ends: the scan is linear.
    static func patternSpans(_ bytes: UnsafeRawBufferPointer) -> [Range<Int>] {
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

    /// The secrets the process has seen, held as none of them, as in Go: a secret is kept as its UTF-8 length and two
    /// polynomial hashes of its bytes, modulo the Mersenne prime 2^61-1, with bases drawn at random for the process.
    /// For two different byte strings of length L, a base drawn at random makes their hashes equal with probability
    /// at most L/2^61, so text matches a secret it differs from with probability below (L/2^61)^2 per window: about
    /// 2^-90 at 64 KiB. Bytes, not `String`s: `String` equality is canonical equivalence, and a secret spelled with a
    /// combining mark is not its precomposed twin. The bases sit beside the hashes, so a short or guessable value can
    /// be found again by trying candidates; a credential cannot.
    final class Registry: @unchecked Sendable {
        struct LengthSet {
            var hashes: Set<Pair> = []
            /// b1^(L-1) and b2^(L-1), which take a byte out of a rolling window.
            let pow1: UInt64, pow2: UInt64
        }

        struct Pair: Hashable {
            let h1: UInt64, h2: UInt64
        }

        private let lock = NSLock()
        private var byLength: [Int: LengthSet] = [:]
        let b1: UInt64, b2: UInt64

        init(b1: UInt64 = Registry.randomBase(), b2: UInt64 = Registry.randomBase()) {
            self.b1 = b1
            self.b2 = b2
        }

        static let mersenne: UInt64 = 1 << 61 - 1

        /// a*b mod 2^61-1, for a and b below the modulus.
        static func mulMod(_ a: UInt64, _ b: UInt64) -> UInt64 {
            let (hi, lo) = a.multipliedFullWidth(by: b)
            var r = (hi << 3 | lo >> 61) + (lo & mersenne)  // a*b = q*2^61 + r, and 2^61 is 1 modulo 2^61-1
            if r >= mersenne { r -= mersenne }
            if r >= mersenne { r -= mersenne }
            return r
        }

        static func addMod(_ a: UInt64, _ b: UInt64) -> UInt64 {
            let r = a + b
            return r >= mersenne ? r - mersenne : r
        }

        static func subMod(_ a: UInt64, _ b: UInt64) -> UInt64 { a >= b ? a - b : a + mersenne - b }

        /// A base in [1, 2^61-2] from the system generator, which cannot fail.
        static func randomBase() -> UInt64 {
            var g = SystemRandomNumberGenerator()
            return UInt64.random(in: 1...(mersenne - 2), using: &g)
        }

        /// The bytes' two hashes: the sum of bytes[i]*b^(n-1-i), modulo 2^61-1, for each base.
        func pair<C: Sequence>(_ bytes: C) -> Pair where C.Element == UInt8 {
            var h1: UInt64 = 0, h2: UInt64 = 0
            for c in bytes {
                h1 = Self.addMod(Self.mulMod(h1, b1), UInt64(c))
                h2 = Self.addMod(Self.mulMod(h2, b2), UInt64(c))
            }
            return Pair(h1: h1, h2: h2)
        }

        func emptySet(length n: Int) -> LengthSet {
            var p1: UInt64 = 1, p2: UInt64 = 1
            for _ in 0..<max(n - 1, 0) {
                p1 = Self.mulMod(p1, b1)
                p2 = Self.mulMod(p2, b2)
            }
            return LengthSet(pow1: p1, pow2: p2)
        }

        func add(_ s: String) {
            let n = s.utf8.count
            let h = pair(s.utf8)  // read in place: no copy of the secret is made or kept
            lock.lock()
            defer { lock.unlock() }
            if byLength[n] == nil { byLength[n] = emptySet(length: n) }
            byLength[n]!.hashes.insert(h)
        }

        /// Every window of `bytes` equal to a registered secret, overlapping ones included.
        func spans(in bytes: UnsafeRawBufferPointer) -> [Range<Int>] {
            lock.lock()
            let sets = byLength
            lock.unlock()
            var found: [Range<Int>] = []
            for (n, set) in sets { found += occurrences(in: bytes, length: n, set: set) }
            return found
        }

        /// Every window of `bytes`, `n` long, whose two hashes are those of a secret in `set`. Both hashes roll: each
        /// step takes the leaving byte out and the next one in, in constant time.
        func occurrences(in bytes: UnsafeRawBufferPointer, length n: Int, set: LengthSet) -> [Range<Int>] {
            guard n > 0, n <= bytes.count else { return [] }
            var h = pair(bytes[0..<n])
            var found: [Range<Int>] = []
            var i = 0
            while true {
                if set.hashes.contains(h) { found.append(i..<i + n) }
                if i + n == bytes.count { return found }
                let out = UInt64(bytes[i]), next = UInt64(bytes[i + n])
                h = Pair(
                    h1: Self.addMod(Self.mulMod(Self.subMod(h.h1, Self.mulMod(out, set.pow1)), b1), next),
                    h2: Self.addMod(Self.mulMod(Self.subMod(h.h2, Self.mulMod(out, set.pow2)), b2), next))
                i += 1
            }
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

    /// Registers `bearer` with the scrubber. A non-finite expiry is made definite (``Secret/definite(_:)``): NaN and
    /// minus infinity are long past, so the token counts as expired; plus infinity is the distant future.
    public init(bearer: String, expires: Date?, plan: String = "", source: String = "") {
        Secret.register(bearer)
        self.bearer = bearer
        self.expires = expires.map(Secret.definite)
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
    /// The expiry as a `Date`. A `Date` is a `Double`, which loses whole seconds beyond 2^53; ``expiresAtUnix`` keeps
    /// them.
    public let expiresAt: Date
    /// The expiry in Unix seconds exactly as minted (`expires_at`), as Go keeps it (`ExpiresAtUnix`).
    public let expiresAtUnix: Int64

    /// Registers `bearer` with the scrubber. A non-finite `expiresAt` is made definite first
    /// (``Secret/definite(_:)``), so the secret compares equal to itself and both expiries agree; ``expiresAtUnix``
    /// is the result rounded down to a whole second, saturated at the bounds of Int64.
    public init(bearer: String, expiresAt: Date) {
        Secret.register(bearer)
        self.bearer = bearer
        let at = Secret.definite(expiresAt)
        self.expiresAt = at
        self.expiresAtUnix = Self.unixSeconds(at)
    }

    /// Registers `bearer` with the scrubber and keeps `expiresAtUnix` exactly.
    public init(bearer: String, expiresAtUnix: Int64) {
        Secret.register(bearer)
        self.bearer = bearer
        self.expiresAt = Date(timeIntervalSince1970: TimeInterval(expiresAtUnix))
        self.expiresAtUnix = expiresAtUnix
    }

    /// `date` in whole Unix seconds, rounded down; saturated rather than trapping beyond Int64 (2^63 itself is out
    /// of range, and `Double(Int64.max)` rounds up to it). NaN, which ``Secret/definite(_:)`` never leaves, is the
    /// smallest value: long expired.
    static func unixSeconds(_ date: Date) -> Int64 {
        let t = date.timeIntervalSince1970.rounded(.down)
        if t.isNaN { return .min }
        if t >= 0x1p63 { return .max }
        if t < -0x1p63 { return .min }
        return Int64(t)
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
        a.bearer.utf8.elementsEqual(b.bearer.utf8) && a.expiresAt == b.expiresAt && a.expiresAtUnix == b.expiresAtUnix
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
