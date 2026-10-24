import Foundation
import Testing
@testable import SaySoAI

/// **The model catalogue is the provider's own list, fetched with the reader's key**: the OpenAI shape, the extra
/// metadata OpenRouter gives, the filters, the typed failures, and the in-memory cache with its timestamp.
@Suite(.serialized)
struct ModelCatalogTests {
    // MARK: - The stand-in

    final class Stub: URLProtocol {
        struct State {
            var responder: (@Sendable (URLRequest) -> (Int, Data))?
            var requests: [URLRequest] = []
        }

        static let state = Locked(State())

        static func install(_ responder: @escaping @Sendable (URLRequest) -> (Int, Data)) {
            state.withLock { $0 = State(responder: responder) }
        }

        static var requests: [URLRequest] { state.withLock { $0.requests } }

        override class func canInit(with request: URLRequest) -> Bool { true }
        override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

        override func startLoading() {
            Self.state.withLock { $0.requests.append(request) }
            let response = Self.state.withLock { $0.responder }?(request)
            guard let (status, data) = response else {
                client?.urlProtocol(self, didFailWithError: URLError(.cannotConnectToHost))
                return
            }
            guard let url = request.url,
                  let http = HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1",
                                             headerFields: ["Content-Type": "application/json"]) else {
                client?.urlProtocol(self, didFailWithError: URLError(.badURL))
                return
            }
            client?.urlProtocol(self, didReceive: http, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        }

        override func stopLoading() {}
    }

    static func session() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [Stub.self]
        return URLSession(configuration: config)
    }

    static func request(_ id: String = "deepseek", key: String? = "sk-test") -> ModelCatalog.Request {
        let profile = AIProviderProfile.profile(id: id) ?? AIProviderProfile.custom
        let base = profile.baseURL ?? URL(string: "https://api.example.com/v1")!
        return ModelCatalog.Request(profile: profile, baseURL: base, apiKey: key)
    }

    static func list(_ ids: [String]) -> Data {
        let data: [String: Any] = ["object": "list", "data": ids.map { ["id": $0, "object": "model"] }]
        return (try? JSONSerialization.data(withJSONObject: data)) ?? Data()
    }

    // MARK: - The list

    @Test func anOpenAIListIsReadSortedAndDeduplicated() async throws {
        Stub.install { _ in
            (200, Self.list(["deepseek-v4-pro", "deepseek-flash", "deepseek-flash"]))
        }
        let models = try await ModelCatalog().fetch(Self.request(), session: Self.session())
        #expect(models.map(\.id) == ["deepseek-flash", "deepseek-v4-pro"])
        let sent = try #require(Stub.requests.first)
        #expect(sent.httpMethod == "GET")
        #expect(sent.url?.absoluteString == "https://api.deepseek.com/models")
        #expect(sent.value(forHTTPHeaderField: "Authorization") == "Bearer sk-test")
    }

    /// The catalogue sends the key only to the profile's own origin, and a local profile needs none.
    @Test func aLocalProfileSendsNoAuthorization() async throws {
        Stub.install { _ in (200, Self.list(["llama3.2"])) }
        let models = try await ModelCatalog().fetch(Self.request("ollama", key: nil), session: Self.session())
        #expect(models.map(\.id) == ["llama3.2"])
        let sent = try #require(Stub.requests.first)
        #expect(sent.value(forHTTPHeaderField: "Authorization") == nil)
        #expect(sent.url?.absoluteString == "http://127.0.0.1:11434/v1/models")
    }

    /// OpenRouter's metadata: text-output models kept, an image-output one dropped, an entry with no metadata kept.
    @Test func theTextOutputFilterUsesTheMetadataWhereItExists() async throws {
        let rows: [String: Any] = ["data": [
            ["id": "vendor/text-model", "name": "Text", "context_length": 128_000,
             "architecture": ["output_modalities": ["text"]]],
            ["id": "vendor/image-model", "architecture": ["output_modalities": ["image"]]],
            ["id": "vendor/unknown"],
        ]]
        let body = (try? JSONSerialization.data(withJSONObject: rows)) ?? Data()
        Stub.install { _ in (200, body) }
        let models = try await ModelCatalog().fetch(Self.request("openrouter", key: nil), session: Self.session())
        #expect(models.map(\.id) == ["vendor/text-model", "vendor/unknown"])
        #expect(models.first?.name == "Text")
        #expect(models.first?.contextLength == 128_000)
    }

    // MARK: - The rows

    /// **Filtered before remembered**: an image row must not make a later text row with the same id look like a
    /// duplicate.
    @Test func anImageRowDoesNotSuppressALaterTextRowWithTheSameID() async throws {
        let rows: [String: Any] = ["data": [
            ["id": "vendor/x", "architecture": ["output_modalities": ["image"]]],
            ["id": "vendor/x", "architecture": ["output_modalities": ["text"]]],
        ]]
        let body = (try? JSONSerialization.data(withJSONObject: rows)) ?? Data()
        Stub.install { _ in (200, body) }
        let models = try await ModelCatalog().fetch(Self.request("openrouter", key: nil), session: Self.session())
        #expect(models.map(\.id) == ["vendor/x"])
    }

    /// A context window is a positive whole number; a JSON boolean, a fraction, a negative and a value too large for
    /// `Int` are not windows.
    @Test func aContextWindowIsAPositiveWholeNumber() async throws {
        let rows: [String: Any] = ["data": [
            ["id": "a", "context_length": true],
            ["id": "b", "context_length": 1_024.5],
            ["id": "c", "context_length": -5],
            ["id": "d", "context_length": 2_048],
            ["id": "e", "context_length": 9.3e18],
        ]]
        let body = (try? JSONSerialization.data(withJSONObject: rows)) ?? Data()
        Stub.install { _ in (200, body) }
        let models = try await ModelCatalog().fetch(Self.request(), session: Self.session())
        #expect(models.map(\.contextLength) == [nil, nil, nil, 2_048, nil])
    }

    // MARK: - Typed failures

    @Test func aMissingKeyIsNotARequest() async {
        Stub.install { _ in (200, Self.list(["x"])) }
        await #expect(throws: ModelCatalogError.missingKey) {
            try await ModelCatalog().fetch(Self.request("deepseek", key: nil), session: Self.session())
        }
        #expect(Stub.requests.isEmpty, "a list that needs a key must not be asked without one")
    }

    @Test func statusesMapToTypedFailures() async throws {
        for (status, expected) in [(401, ModelCatalogError.unauthorised), (403, .unauthorised), (404, .noList),
                                   (429, .rateLimited)] {
            Stub.install { _ in (status, Data("{}".utf8)) }
            await #expect(throws: expected) { try await ModelCatalog().fetch(Self.request(), session: Self.session()) }
        }
        Stub.install { _ in
            (400, Data(#"{"error":{"message":"Invalid Parameters"}}"#.utf8))
        }
        await #expect(throws: ModelCatalogError.server(status: 400, message: "Invalid Parameters")) {
            try await ModelCatalog().fetch(Self.request(), session: Self.session())
        }
    }

    /// **The status is read before the body**: a 401 with an endless body is unauthorised, never a bad shape.
    @Test func anOversizedRefusalKeepsItsStatus() async {
        Stub.install { _ in (401, Data(repeating: 0x61, count: ModelCatalog.errorBodyCap * 4)) }
        await #expect(throws: ModelCatalogError.unauthorised) {
            try await ModelCatalog().fetch(Self.request(), session: Self.session())
        }
    }

    @Test func aBodyThatIsNotAListIsABadShape() async throws {
        Stub.install { _ in (200, Data(#"{"choices":[]}"#.utf8)) }
        await #expect(throws: ModelCatalogError.badShape) {
            try await ModelCatalog().fetch(Self.request(), session: Self.session())
        }
    }

    /// Azure's deployments are per resource; a profile with no list path says so before any request.
    @Test func aProfileWithoutAListSaysSo() async {
        let profile = AIProviderProfile(id: "azure", name: "Azure", baseURL: URL(string: "https://example.com"),
                                        auth: .header("api-key"), keyRequired: true, tokenField: .maxTokensFirst,
                                        modelsPath: nil, modelsNeedKey: true, modelsSendKey: true,
                                        modelFilter: .all)
        let request = ModelCatalog.Request(profile: profile, baseURL: profile.baseURL!, apiKey: "k")
        await #expect(throws: ModelCatalogError.noList) {
            try await ModelCatalog().fetch(request, session: Self.session())
        }
    }

    // MARK: - The cache

    @Test func theCacheKeepsATimestampAndExpires() async {
        let cache = ModelCatalogCache(ttl: 60)
        let request = Self.request()
        let now = Date()
        await cache.store([AIModel(id: "a")], for: request, now: now)
        let fresh = await cache.entry(for: request, now: now.addingTimeInterval(30))
        let stale = await cache.entry(for: request, now: now.addingTimeInterval(61))
        #expect(fresh?.models == [AIModel(id: "a")])
        #expect(stale == nil)
    }

    /// **Two keys may see different lists**, so a fetch made with one never answers for the other; and an expired entry
    /// is dropped rather than kept for good.
    @Test func theCacheKeepsKeysApartAndPrunesExpiredEntries() async {
        let cache = ModelCatalogCache(ttl: 60)
        let profile = AIProviderProfile.profile(id: "deepseek") ?? AIProviderProfile.custom
        let url = profile.baseURL ?? URL(string: "https://api.example.com/v1")!
        let a = ModelCatalog.Request(profile: profile, baseURL: url, apiKey: "key-a")
        let b = ModelCatalog.Request(profile: profile, baseURL: url, apiKey: "key-b")
        let now = Date()
        await cache.store([AIModel(id: "a")], for: a, now: now)
        #expect(await cache.entry(for: b, now: now) == nil, "one key answered for another")
        await cache.store([AIModel(id: "b")], for: b, now: now.addingTimeInterval(61))
        #expect(await cache.entry(for: a, now: now.addingTimeInterval(62)) == nil)
    }

    /// **The list's own requirements are part of the key**: a profile made to require a key is a different request.
    @Test func theCacheKeyCoversTheListsRequirements() throws {
        let base = try #require(AIProviderProfile.profile(id: "custom"))
        let needing = AIProviderProfile(
            id: base.id, name: base.name, baseURL: base.baseURL, auth: base.auth, keyRequired: base.keyRequired,
            tokenField: base.tokenField, modelsPath: base.modelsPath, modelsNeedKey: true,
            modelsSendKey: base.modelsSendKey, modelFilter: base.modelFilter, hints: base.hints, note: base.note)
        let url = try #require(URL(string: "https://api.example.com/v1"))
        let a = ModelCatalog.Request(profile: base, baseURL: url, apiKey: "k")
        let b = ModelCatalog.Request(profile: needing, baseURL: url, apiKey: "k")
        #expect(ModelCatalogCache.key(a) != ModelCatalogCache.key(b))
    }

    // MARK: - The table

    @Test func everyKnownProfileIsRoutable() {
        #expect(!AIProviderProfile.known.isEmpty)
        for profile in AIProviderProfile.known where profile.id != "custom" {
            let url = profile.baseURL
            #expect(url != nil, "\(profile.id) has no base address")
            if let url {
                #expect(url.scheme == "https" || (url.scheme == "http" && ["localhost", "127.0.0.1"].contains(url.host ?? "")),
                        "\(profile.id) is neither https nor loopback")
            }
        }
        #expect(AIProviderProfile.profile(id: "deepseek")?.hints.contains("deepseek-flash") == true)
        #expect(AIProviderProfile.profile(id: "custom")?.baseURL == nil)
    }
}
