import Foundation

/// The process-wide scrubber (spec/credentials.md, hygiene). It removes every registered secret, longest first, and
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
    /// that may reach a user, a log or an artifact.
    public static func scrub(_ text: String) -> String {
        var s = text
        for secret in registry.ordered() where s.contains(secret) {
            s = s.replacingOccurrences(of: secret, with: redacted)
        }
        return replacePatterns(s)
    }

    /// `eyJ[\w-]+\.[\w-]+\.[\w-]+|ek_[\w-]+`, where `\w` is ASCII `[A-Za-z0-9_]` as in Go's RE2.
    private static func replacePatterns(_ s: String) -> String {
        let bytes = Array(s.utf8)
        func isWord(_ b: UInt8) -> Bool {
            (b >= 0x30 && b <= 0x39) || (b >= 0x41 && b <= 0x5A) || (b >= 0x61 && b <= 0x7A) || b == 0x5F || b == 0x2D
        }
        func run(from i: Int) -> Int {
            var j = i
            while j < bytes.count, isWord(bytes[j]) { j += 1 }
            return j
        }
        func has(_ prefix: [UInt8], at i: Int) -> Bool {
            i + prefix.count <= bytes.count && Array(bytes[i..<i + prefix.count]) == prefix
        }
        let jwt = Array("eyJ".utf8), ek = Array("ek_".utf8)
        var out: [UInt8] = []
        out.reserveCapacity(bytes.count)
        var i = 0
        while i < bytes.count {
            if has(jwt, at: i) {
                // eyJ, then three dot-separated non-empty runs (the first continues the eyJ run).
                let a = run(from: i + 3)
                if a > i + 3, a < bytes.count, bytes[a] == 0x2E {
                    let b = run(from: a + 1)
                    if b > a + 1, b < bytes.count, bytes[b] == 0x2E {
                        let c = run(from: b + 1)
                        if c > b + 1 {
                            out.append(contentsOf: redacted.utf8)
                            i = c
                            continue
                        }
                    }
                }
            }
            if has(ek, at: i) {
                let e = run(from: i + 3)
                if e > i + 3 {
                    out.append(contentsOf: redacted.utf8)
                    i = e
                    continue
                }
            }
            out.append(bytes[i])
            i += 1
        }
        return String(decoding: out, as: UTF8.self)
    }

    private final class Registry: @unchecked Sendable {
        private let lock = NSLock()
        private var known: Set<String> = []
        private var list: [String] = []  // longest first, so a secret containing another is removed whole

        func add(_ s: String) {
            lock.lock()
            defer { lock.unlock() }
            guard known.insert(s).inserted else { return }
            let at = list.firstIndex { $0.utf8.count < s.utf8.count } ?? list.endIndex
            list.insert(s, at: at)
        }

        func ordered() -> [String] {
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
