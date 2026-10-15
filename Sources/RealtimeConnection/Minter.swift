import Foundation

/// Mints ephemeral client secrets (spec/http-and-mint.md, `NET-02`).
public struct Minter: SecretSource {
    public static let path = "/v1/realtime/client_secrets"
    /// The mint reply is at most this long; a longer reply is `protocol`.
    public static let maximumReply = 1 << 20
    /// The minted value is at most this many UTF-8 bytes.
    public static let maximumSecretBytes = 4096

    public let client: HTTPClient
    public let credentials: any CredentialSource
    public let baseURL: URL
    /// Per request, in seconds.
    public let timeout: TimeInterval
    /// The secret's asked lifetime, in whole seconds.
    public let lifetimeSeconds: Int
    /// Named on a route-denied error; optional.
    public let route: String

    /// `timeout` (seconds, per request) and `lifetimeSeconds` are required and positive. `baseURL` is the real origin
    /// unless a test passes one its client's policy allows.
    public init(
        client: HTTPClient, credentials: any CredentialSource, timeout: TimeInterval, lifetimeSeconds: Int,
        baseURL: URL = OriginPolicy.apiBase, route: String = ""
    ) {
        self.client = client
        self.credentials = credentials
        self.timeout = timeout
        self.lifetimeSeconds = lifetimeSeconds
        self.baseURL = baseURL
        self.route = route
    }

    /// The mint body for `session`, the caller's JSON object passed through byte for byte:
    /// `{"session": <session>, "expires_after": {"anchor": "created_at", "seconds": <lifetime>}}`.
    public static func body(session: Data, lifetimeSeconds: Int) throws -> Data {
        guard lifetimeSeconds > 0 else {
            throw RealtimeError(.internal, message: "lifetime \(lifetimeSeconds) s is not a positive whole number of seconds")
        }
        guard (try? JSONSerialization.jsonObject(with: session)) is [String: Any] else {
            throw RealtimeError(.internal, message: "the session is not a JSON object")
        }
        var body = Data(#"{"session":"#.utf8)
        body.append(session)
        body.append(Data(#","expires_after":{"anchor":"created_at","seconds":\#(lifetimeSeconds)}}"#.utf8))
        return body
    }

    /// Mints a secret for `session`, an encoded JSON object (the session shape is the app's). On `auth-expired` it asks
    /// the credentials to refresh after that token and retries once; a second `auth-expired` is `auth-expired`.
    public func secret(session: Data) async throws -> ClientSecret {
        guard timeout > 0 else { throw RealtimeError(.internal, message: "Minter needs a positive timeout") }
        let body = try Self.body(session: session, lifetimeSeconds: lifetimeSeconds)
        let token = try await credentials.token()
        do {
            return try await post(token, body)
        } catch let e as RealtimeError where e.errorClass == .authExpired {
            let fresh = try await credentials.refresh(after: token)
            do {
                return try await post(fresh, body)
            } catch let again as RealtimeError where again.errorClass == .authExpired {
                var refused = again
                refused.message = "the refreshed sign-in was refused too"
                refused.cause = again.description
                throw refused
            }
        }
    }

    /// Mints a secret for an `Encodable` session, encoded without escaping slashes.
    public func secret<Session: Encodable>(encoding session: Session) async throws -> ClientSecret {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        let data: Data
        do { data = try encoder.encode(session) } catch {
            throw RealtimeError(.internal, message: "the session does not encode")
        }
        return try await secret(session: data)
    }

    /// One mint request. Neither the request nor the reply is ever logged: both carry or yield secrets.
    private func post(_ token: Token, _ body: Data) async throws -> ClientSecret {
        var url = baseURL.absoluteString
        while url.hasSuffix("/") { url.removeLast() }
        guard let endpoint = URL(string: url + Self.path) else {
            throw RealtimeError(.internal, message: "the base URL does not form a mint URL")
        }
        var request = URLRequest(url: endpoint)
        request.httpMethod = "POST"
        request.setValue("Bearer " + token.bearer, forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = body
        var reply = try await client.send(request, timeout: timeout, limit: Self.maximumReply, route: route).body
        defer { reply.resetBytes(in: 0..<reply.count) }  // the reply holds the secret
        guard let object = try? JSONSerialization.jsonObject(with: reply, options: [.fragmentsAllowed]) else {
            throw RealtimeError(.protocol, message: "mint reply is not valid JSON")
        }
        let fields = object as? [String: Any]
        guard let fields, let rawExpiry = fields["expires_at"], !(rawExpiry is NSNull),
            let expires = JSONField.int(rawExpiry)
        else { throw RealtimeError(.protocol, message: "mint reply lacks an integer expires_at") }
        guard let value = fields["value"] as? String, !value.isEmpty, value.utf8.count <= Self.maximumSecretBytes else {
            throw RealtimeError(.protocol, message: "mint reply value is missing, empty or longer than 4096 bytes")
        }
        return ClientSecret(bearer: value, expiresAt: Date(timeIntervalSince1970: TimeInterval(expires)))
    }
}
