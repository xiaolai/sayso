import Foundation

/// One model a service advertises: its id always, a display name and context window where the service gives them.
public struct AIModel: Sendable, Equatable, Identifiable {
    public let id: String
    public let name: String?
    public let contextLength: Int?

    public init(id: String, name: String? = nil, contextLength: Int? = nil) {
        self.id = id
        self.name = name
        self.contextLength = contextLength
    }
}

/// Why a model list could not be read. Kept apart from `AIProviderError`: asking for a list is a configuration aid, not
/// an answer, and its failures are worded differently (a missing key is not a refused key).
public enum ModelCatalogError: Error, Equatable, Sendable {
    /// The profile has no model list at all (Azure's deployments are per resource; `.custom` may still list one).
    case noList
    /// The list request needs a key and none was given yet.
    case missingKey
    case unreachable
    case unauthorised
    /// The service throttled the list request.
    case rateLimited
    case timeout
    case cancelled
    /// The server refused the list request with a status this library has no typed meaning for, and its own words.
    case server(status: Int, message: String)
    /// A 2xx whose body is not a model list.
    case badShape
}

/// **A service's current model list, fetched from the service itself**: `GET <base>/<modelsPath>` with the profile's
/// auth, parsed from the OpenAI shape (and OpenRouter's extra metadata), filtered, sorted. It is a snapshot with a
/// time, not a promise: a model listed here may still refuse the reader's key, and only an ask proves otherwise.
public struct ModelCatalog: Sendable {
    /// The request the reader configured: a profile, the address to ask and, when the service wants one, a key.
    public struct Request: Sendable, Equatable {
        public let profile: AIProviderProfile
        public let baseURL: URL
        public let apiKey: String?

        public init(profile: AIProviderProfile, baseURL: URL, apiKey: String?) {
            self.profile = profile
            self.baseURL = baseURL
            self.apiKey = apiKey
        }
    }

    public init() {}

    /// The most body a list may carry: OpenRouter's full catalogue is a few hundred KiB.
    public static let responseCap = 2 << 20
    /// How much of a refusal's body is read for its sentence.
    public static let errorBodyCap = 64 << 10

    public func fetch(_ request: Request, session: URLSession? = nil) async throws(ModelCatalogError) -> [AIModel] {
        let (urlRequest, origin) = try Self.prepare(request)
        let (status, body) = try await Self.transport(urlRequest, origin: origin, session: session ?? Self.session())
        try Self.statusError(status, body: body.data, truncated: body.truncated, key: request.apiKey)
        return try Self.models(in: body.data, filter: request.profile.modelFilter)
    }

    // MARK: - The request

    static func prepare(_ request: Request) throws(ModelCatalogError) -> (URLRequest, Origin) {
        guard let path = request.profile.modelsPath else { throw .noList }
        if request.profile.modelsNeedKey, (request.apiKey ?? "").isEmpty { throw .missingKey }
        guard let origin = Origin(request.baseURL), origin.isAcceptable else { throw .unreachable }
        var urlRequest = URLRequest(url: request.baseURL.appending(path: path))
        urlRequest.httpMethod = "GET"
        // **A public list is not given the key**: a saved credential is for completion, and a list that does not ask
        // for one has no business receiving it.
        if request.profile.modelsSendKey {
            Wire.authorize(&urlRequest, auth: request.profile.auth, key: request.apiKey)
        }
        return (urlRequest, origin)
    }

    static func session() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 30
        config.timeoutIntervalForResource = 60
        return URLSession(configuration: config)
    }

    // MARK: - The wire

    static func transport(_ request: URLRequest, origin: Origin,
                          session: URLSession) async throws(ModelCatalogError) -> (Int, WireBody) {
        let delegate = HTTPProvider.RedirectGuard(origin: origin)
        do {
            let (bytes, response) = try await session.bytes(for: request, delegate: delegate)
            guard let http = response as? HTTPURLResponse else { throw ModelCatalogError.badShape }
            if delegate.refused { throw ModelCatalogError.unreachable }
            guard origin == Origin(http.url ?? request.url) else { throw ModelCatalogError.unreachable }
            // **The status comes before the body, and decides whether there is one**: a 401 answered with a huge body
            // is unauthorised, never a bad shape, and its transfer is cancelled unread. Only a 2xx is read whole; a
            // refusal's sentence is short, best-effort, and its stream is cut at the ceiling. (A body that never starts
            // cannot be told from an unanswered request by URLSession; that one remains `.timeout`.)
            let status = http.statusCode
            if (200..<300).contains(status) {
                return (status, try await Wire.collect(bytes, cap: Self.responseCap))
            }
            if HTTPProvider.needsNoBody(status) {
                bytes.task.cancel()
                return (status, WireBody(data: Data(), truncated: false))
            }
            // A broken body keeps the status: its sentence is lost, the server's verdict is not. The caller's
            // cancellation is never swallowed.
            guard let body = await Wire.collectForMessage(bytes, cap: Self.errorBodyCap) else {
                throw ModelCatalogError.cancelled
            }
            return (status, body)
        } catch let error as ModelCatalogError {
            throw error
        } catch let failure as WireFailure {
            switch failure {
            case .tooLarge, .unexpected: throw .badShape
            case .cancelled: throw .cancelled
            case .transport(let code): throw Self.transport(code)
            }
        } catch let error as URLError {
            throw Self.transport(error.code)
        } catch is CancellationError {
            throw .cancelled
        } catch {
            throw .unreachable
        }
    }

    static func transport(_ code: URLError.Code) -> ModelCatalogError {
        switch code {
        case .cancelled: .cancelled
        case .timedOut: .timeout
        default: .unreachable
        }
    }

    static func statusError(_ status: Int, body: Data, truncated: Bool = false, key: String?) throws(ModelCatalogError) {
        switch status {
        case 200..<300: return
        case 401, 403: throw .unauthorised
        case 404: throw .noList
        case 429: throw .rateLimited
        default: throw .server(status: status, message: HTTPProvider.message(in: body, truncated: truncated, key: key))
        }
    }

    // MARK: - The list

    /// **The OpenAI list shape**, plus the members some services add: `name`, `context_length`, and OpenRouter's
    /// `architecture.output_modalities`. A list with no modality metadata is kept whole — hiding a model that would
    /// have answered is worse than showing one that does not.
    static func models(in data: Data, filter: AIProviderModelFilter) throws(ModelCatalogError) -> [AIModel] {
        guard let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let rows = object["data"] as? [[String: Any]] else { throw .badShape }
        var models: [AIModel] = []
        var seen = Set<String>()
        for row in rows {
            guard let id = row["id"] as? String, !id.isEmpty else { continue }
            // **Filter before remembering an id**: an image row must not make a later text row with the same id look
            // like a duplicate.
            if filter == .textOutput, !takesText(row) { continue }
            guard seen.insert(id).inserted else { continue }
            let context = positiveInt(row["context_length"]) ?? positiveInt(row["context_window"])
            models.append(AIModel(id: id, name: row["name"] as? String, contextLength: context))
        }
        return models.sorted { $0.id.localizedStandardCompare($1.id) == .orderedAscending }
    }

    static func takesText(_ row: [String: Any]) -> Bool {
        guard let architecture = row["architecture"] as? [String: Any],
              let outputs = architecture["output_modalities"] as? [String] else { return true }
        return outputs.contains("text")
    }

    /// A context window: a positive whole number. JSON booleans arrive as `NSNumber` too, and `true` is not a window.
    /// The bridge to `Int` is exact, never through `Double`: it refuses a fraction or a value past `Int`, and keeps every
    /// digit of one that fits.
    static func positiveInt(_ value: Any?) -> Int? {
        guard let number = value as? NSNumber, type(of: number) != Self.booleanType,
              let int = number as? Int, int >= 1 else { return nil }
        return int
    }

    private static let booleanType = type(of: NSNumber(value: true))
}

extension ModelCatalog.Request: CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public var description: String { "ModelCatalog.Request(\(profile.id), \(AIProviderID.scrubbed(baseURL).absoluteString))" }
    public var debugDescription: String { description }
    public var customMirror: Mirror { Mirror(self, children: [:]) }
}

/// **An in-memory list cache**: the shell persists nothing here; a process keeps the last fetch per profile, address
/// and key with its time, and may show it before refreshing. A fetch is cheap but not free — do it on the reader's
/// actions, never per keystroke.
public actor ModelCatalogCache {
    public struct Entry: Sendable, Equatable {
        public let models: [AIModel]
        public let fetchedAt: Date

        public init(models: [AIModel], fetchedAt: Date) {
            self.models = models
            self.fetchedAt = fetchedAt
        }
    }

    private var entries: [String: Entry] = [:]
    public let ttl: TimeInterval

    public init(ttl: TimeInterval = 24 * 60 * 60) {
        self.ttl = ttl
    }

    /// **Everything that can change the answer is in the key**: the profile, its address, the list's path and filter,
    /// how the key travels, and the key itself. Two keys may see different lists (a paid tier's models, a revoked
    /// key's), so one fetch must not answer for the other. The key never enters the cache key — its process-local hash
    /// distinguishes two without holding either, and an in-memory cache needs no hash stable across launches.
    static func key(_ request: ModelCatalog.Request) -> String {
        let fingerprint = request.apiKey.map { String($0.hashValue) } ?? "-"
        return [request.profile.id, request.baseURL.absoluteString, request.profile.modelsPath ?? "-",
                String(describing: request.profile.modelFilter), String(describing: request.profile.auth),
                request.profile.modelsNeedKey ? "need" : "free", request.profile.modelsSendKey ? "send" : "quiet",
                fingerprint].joined(separator: "|")
    }

    public func entry(for request: ModelCatalog.Request, now: Date = Date()) -> Entry? {
        let key = Self.key(request)
        guard let entry = entries[key] else { return nil }
        guard now.timeIntervalSince(entry.fetchedAt) <= ttl else {
            entries[key] = nil
            return nil
        }
        return entry
    }

    public func store(_ models: [AIModel], for request: ModelCatalog.Request, now: Date = Date()) {
        // Expired entries are dropped on the way in, so a long-lived process does not accumulate one per key ever seen.
        entries = entries.filter { now.timeIntervalSince($0.value.fetchedAt) <= ttl }
        entries[Self.key(request)] = Entry(models: models, fetchedAt: now)
    }

    public func clear() {
        entries.removeAll()
    }
}
