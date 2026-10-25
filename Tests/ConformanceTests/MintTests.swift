import Foundation
import RealtimeConnection
import XCTest

final class MintTests: XCTestCase {
    func testMint() throws { try runFixtures("mint") { c in FakeOpenAI.outcome(try MintCheck.run(c.object)) } }
}

/// Returns `token`, and `refreshTo` after a refresh, counting both.
final class StubCredentials: CredentialSource, @unchecked Sendable {
    private let lock = NSLock()
    private let tokenValue: String, refreshValue: String
    private var tokenCount = 0
    private var refreshCount = 0

    init(token: String, refreshTo: String) {
        tokenValue = token
        refreshValue = refreshTo
    }

    var tokens: Int { lock.withLock { tokenCount } }
    var refreshes: Int { lock.withLock { refreshCount } }

    func token() async throws -> Token {
        lock.withLock { tokenCount += 1 }
        return Token(bearer: tokenValue, expires: nil, source: "conformance")
    }

    func refresh(after stale: Token) async throws -> Token {
        lock.withLock { refreshCount += 1 }
        return Token(bearer: refreshValue, expires: nil, source: "conformance")
    }
}

enum MintCheck {
    static func run(_ c: [String: Any]) throws -> [String] {
        guard let setup = c["setup"] as? [String: Any], let want = c["expect"] as? [String: Any] else {
            throw FixtureError("mint case without setup or expect")
        }
        var unknown = FakeOpenAI.unknownKeys("case", c, FakeOpenAI.caseHead.union(["setup", "expect"]))
        unknown += FakeOpenAI.unknownKeys(
            "setup", setup, ["now", "origin", "session", "lifetimeSeconds", "token", "refreshTo", "replies"])
        for (i, r) in (setup["replies"] as? [[String: Any]] ?? []).enumerated() {
            unknown += FakeOpenAI.unknownKeys("replies[\(i)]", r, ["status", "headers", "json", "body", "bodyPadTo", "hangup"])
        }
        unknown += FakeOpenAI.unknownKeys(
            "expect", want,
            [
                "class", "secret", "expiresAt", "status", "type", "code", "param", "requestId", "requests",
                "refreshCalls", "tokenCalls", "request", "secondAuthorization", "scrubs", "notInDescription",
            ])
        unknown += FakeOpenAI.unknownKeys(
            "expect.request", want["request"] as? [String: Any],
            ["method", "path", "authorization", "contentType", "body"])
        guard unknown.isEmpty else { return unknown }
        let token = "\(Fixtures.substitute(setup["token"] ?? ""))"
        let refreshTo = "\(Fixtures.substitute(setup["refreshTo"] ?? ""))"
        var replies: [FakeHTTPReply] = []
        for r in setup["replies"] as? [[String: Any]] ?? [] {
            var body = Data()
            if let text = r["body"] as? String {
                body = Data(text.utf8)
            } else if let json = r["json"] {
                body = try JSONSerialization.data(withJSONObject: json, options: [.fragmentsAllowed, .withoutEscapingSlashes])
            }
            if let pad = r["bodyPadTo"] as? Int, pad > body.count { body.append(Data(repeating: 0x20, count: pad - body.count)) }
            let headers = (r["headers"] as? [String: String] ?? [:]).sorted { $0.key < $1.key }.map { ($0.key, $0.value) }
            replies.append(
                FakeHTTPReply(
                    status: r["status"] as? Int ?? 0, headers: headers, body: body, hangup: r["hangup"] as? Bool ?? false))
        }
        let server = try FakeHTTPServer(replies: replies)
        let port = try server.start()
        defer { server.stop() }
        let origin = setup["origin"] as? String ?? "test"
        guard let base = URL(string: origin == "test" ? "http://127.0.0.1:\(port)" : origin) else {
            throw FixtureError("origin \(origin) is not a URL")
        }
        let client = try FakeOpenAI.client(port: port)
        let creds = StubCredentials(token: token, refreshTo: refreshTo)
        let session = try JSONSerialization.data(withJSONObject: setup["session"] ?? [:])
        let minter = Minter(
            client: client, credentials: creds, timeout: 10, lifetimeSeconds: setup["lifetimeSeconds"] as? Int ?? 0,
            baseURL: base)
        let result = FakeOpenAI.blocking { try await minter.secret(session: session) }
        let requests = server.requests

        var p: [String] = []
        if want["class"] == nil || want["class"] is NSNull {
            switch result {
            case .failure(let error): return ["mint failed: \(error)"]
            case .success(let secret):
                if let w = want["secret"], !FakeOpenAI.same(secret.bearer, w) { p.append("secret differs") }
                let at = secret.expiresAtUnix
                if let w = want["expiresAt"], !FakeOpenAI.same(at, w) { p.append("expiresAt \(at), want \(w)") }
            }
        } else {
            var error: Error?
            if case .failure(let e) = result { error = e }
            p += FakeOpenAI.checkError(error, ["class": want["class"] as Any])
            if let e = error as? RealtimeError {
                let fields: [(String, Any)] = [
                    ("status", e.status), ("type", e.type), ("code", e.code), ("param", e.param), ("requestId", e.requestID),
                ]
                for (key, got) in fields {
                    if let w = want[key], !FakeOpenAI.same(got, w) { p.append("\(key) \(got), want \(w)") }
                }
            }
            if let error { p += FakeOpenAI.leaks(FakeOpenAI.strings(want["notInDescription"]), String(describing: error)) }
        }
        if let w = want["requests"], !FakeOpenAI.same(requests.count, w) { p.append("requests \(requests.count), want \(w)") }
        if let w = want["refreshCalls"], !FakeOpenAI.same(creds.refreshes, w) {
            p.append("refreshCalls \(creds.refreshes), want \(w)")
        }
        if let w = want["tokenCalls"], !FakeOpenAI.same(creds.tokens, w) {
            p.append("tokenCalls \(creds.tokens), want \(w)")
        }
        if let w = want["request"] as? [String: Any] {
            if let r = requests.first {
                let body = (try? JSONSerialization.jsonObject(with: r.body)) ?? NSNull()
                let got: [String: Any] = [
                    "method": r.method, "path": r.path, "authorization": r.headers["authorization"] ?? "",
                    "contentType": r.headers["content-type"] ?? "", "body": body,
                ]
                for (key, value) in w {
                    let v = key == "authorization" ? "\(value)".replacingOccurrences(of: "$token", with: token) : value
                    if !FakeOpenAI.same(got[key], v) { p.append("request \(key) = \(got[key] ?? "nil"), want \(v)") }
                }
            } else {
                p.append("no request")
            }
        }
        if let w = want["secondAuthorization"] {
            let expected = "\(w)".replacingOccurrences(of: "$refreshTo", with: refreshTo)
                .replacingOccurrences(of: "$token", with: token)
            if requests.count < 2 || requests[1].headers["authorization"] != expected {
                p.append("second request did not carry the expected token")
            }
        }
        for s in FakeOpenAI.strings(want["scrubs"]) where Secret.scrub("x \(s) y").contains(s) {
            p.append("scrubber does not remove \"\(s)\"")
        }
        return p
    }
}

/// The origin policy beyond the fixtures: what may carry a bearer, and that test origins stay on this machine.
final class MintPolicyTests: XCTestCase {
    func request(_ url: String, host: String? = nil) -> URLRequest {
        var r = URLRequest(url: URL(string: url)!)
        r.setValue("Bearer x", forHTTPHeaderField: "Authorization")
        if let host { r.setValue(host, forHTTPHeaderField: "Host") }
        return r
    }

    func testRealOriginOnly() throws {
        let policy = OriginPolicy()
        XCTAssertNoThrow(try policy.check(request("https://api.openai.com/v1/realtime/client_secrets")))
        XCTAssertNoThrow(try policy.check(request("https://api.openai.com:443/v1/x")))
        XCTAssertNoThrow(try policy.check(request("wss://api.openai.com/v1/realtime?model=m")))
        for bad in [
            "http://api.openai.com/x", "https://api.openai.com:8443/x", "https://user@api.openai.com/x",
            "https://api.openai.com.evil.invalid/x", "ws://api.openai.com/x", "http://127.0.0.1:9/x",
        ] {
            XCTAssertThrowsError(try policy.check(request(bad)), bad) { e in
                XCTAssertEqual((e as? RealtimeError)?.errorClass, .internal, bad)
            }
        }
        XCTAssertThrowsError(try policy.check(request("https://api.openai.com/x", host: "evil.invalid")))
        var plain = URLRequest(url: URL(string: "http://elsewhere.invalid/")!)
        plain.httpMethod = "GET"
        XCTAssertNoThrow(try policy.check(plain), "a request without Authorization is not checked")
    }

    func testTestOriginsAreLoopback() throws {
        XCTAssertThrowsError(try OriginPolicy(testOrigins: ["example.com:443"]))
        XCTAssertThrowsError(try OriginPolicy(testOrigins: ["10.0.0.1:80"]))
        XCTAssertThrowsError(try OriginPolicy(testOrigins: ["127.0.0.1"]))
        let policy = try OriginPolicy(testOrigins: ["127.0.0.1:8080", "localhost:9"])
        XCTAssertNoThrow(try policy.check(request("http://127.0.0.1:8080/x")))
        XCTAssertNoThrow(try policy.check(request("ws://localhost:9/x")))
        XCTAssertThrowsError(try policy.check(request("http://127.0.0.1:8081/x")))
    }

    func testRealNetworkGuard() throws {
        let policy = OriginPolicy(realNetworkGuard: { "unit tests" })
        XCTAssertThrowsError(try policy.check(request("https://api.openai.com/x"))) { e in
            XCTAssertEqual((e as? RealtimeError)?.errorClass, .internal)
        }
        let test = try OriginPolicy(testOrigins: ["127.0.0.1:1"], realNetworkGuard: { "unit tests" })
        XCTAssertNoThrow(try test.check(request("http://127.0.0.1:1/x")), "the guard applies to the real origin only")
    }

    func testMintBodyPassesTheSessionThrough() throws {
        let session = Data(#"{"type":"realtime","instructions":"a/b <c>"}"#.utf8)
        let body = try Minter.body(session: session, lifetimeSeconds: 60)
        XCTAssertEqual(
            String(decoding: body, as: UTF8.self),
            #"{"session":{"type":"realtime","instructions":"a/b <c>"},"expires_after":{"anchor":"created_at","seconds":60}}"#)
        XCTAssertThrowsError(try Minter.body(session: session, lifetimeSeconds: 0))
        XCTAssertThrowsError(try Minter.body(session: Data("[1]".utf8), lifetimeSeconds: 60))
    }

    /// A base URL with a query or a fragment would swallow the mint path into it, so the request would go elsewhere
    /// on an allowed origin: it is a programming error, refused before any sign-in is spent.
    func testABaseURLWithAQueryOrFragmentIsRefused() async throws {
        let client = HTTPClient(policy: try OriginPolicy(testOrigins: ["127.0.0.1:1"]))
        for base in ["http://127.0.0.1:1?x=1", "http://127.0.0.1:1/#f"] {
            let creds = StubCredentials(token: "opaque-base-url-token", refreshTo: "")
            let minter = Minter(
                client: client, credentials: creds, timeout: 1, lifetimeSeconds: 60, baseURL: URL(string: base)!)
            do {
                _ = try await minter.secret(session: Data("{}".utf8))
                XCTFail("\(base) minted")
            } catch {
                XCTAssertEqual(ErrorClass.of(error), .internal, "\(base): \(error)")
            }
            XCTAssertEqual(creds.tokens, 0, "\(base) cost a sign-in")
        }
    }

    /// A session that does not encode says why, in the cause.
    func testASessionThatDoesNotEncodeKeepsTheReason() async throws {
        struct Session: Encodable { let temperature = Double.nan }
        let minter = Minter(
            client: HTTPClient(), credentials: StubCredentials(token: "t", refreshTo: ""), timeout: 1,
            lifetimeSeconds: 60)
        do {
            _ = try await minter.secret(encoding: Session())
            XCTFail("a NaN encoded")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .internal)
            XCTAssertTrue(e.cause.contains("temperature"), e.cause)
        }
    }

    func testSecretsFormatRedacted() {
        let token = Token(bearer: "opaque-bearer-value", expires: nil)
        let secret = ClientSecret(bearer: "ek_formatted", expiresAt: Date())
        for text in ["\(token)", "\(secret)", String(reflecting: token), String(reflecting: secret), "\([token])"] {
            XCTAssertFalse(text.contains("opaque-bearer-value") || text.contains("ek_formatted"), text)
        }
        XCTAssertEqual(Secret.scrub("a opaque-bearer-value b"), "a <redacted> b")
        XCTAssertEqual(Secret.scrub("x eyJa.b-c.d_e y ek_z9 z"), "x <redacted> y <redacted> z")
        XCTAssertEqual(Secret.scrub("eyJ.a.b ek_ eyJa.b"), "eyJ.a.b ek_ eyJa.b")
    }
}
