import Foundation

/// `response.done.status`.
public enum ResponseStatus: String, Sendable, Equatable, CaseIterable {
    case completed, failed, incomplete, cancelled
}

/// The `response.done` usage: the totals and, when the server sends `input_token_details` / `output_token_details`,
/// their text and audio parts.
public struct TokenUsage: Sendable, Equatable {
    public var inputTokens: Int
    public var outputTokens: Int
    public var inputTextTokens: Int
    public var outputTextTokens: Int
    public var outputAudioTokens: Int
    /// Set when `output_token_details` was present, so a zero audio count is real.
    public var detailed: Bool

    public init(
        inputTokens: Int = 0, outputTokens: Int = 0, inputTextTokens: Int = 0, outputTextTokens: Int = 0,
        outputAudioTokens: Int = 0, detailed: Bool = false
    ) {
        self.inputTokens = inputTokens
        self.outputTokens = outputTokens
        self.inputTextTokens = inputTextTokens
        self.outputTextTokens = outputTextTokens
        self.outputAudioTokens = outputAudioTokens
        self.detailed = detailed
    }
}

/// One entry of `rate_limits.updated`.
public struct RateLimit: Sendable, Equatable {
    public var name: String
    public var limit: Int64
    public var remaining: Int64
    public var resetSeconds: Double

    public init(name: String, limit: Int64, remaining: Int64, resetSeconds: Double) {
        self.name = name
        self.limit = limit
        self.remaining = remaining
        self.resetSeconds = resetSeconds
    }
}

/// `response.done`.
public struct ResponseDone: Sendable, Equatable {
    public var responseID: String
    public var status: ResponseStatus
    /// `status_details.reason`, "" when absent.
    public var statusReason: String
    public var usage: TokenUsage
    public var metadata: [String: String]?

    public init(
        responseID: String, status: ResponseStatus, statusReason: String = "", usage: TokenUsage = TokenUsage(),
        metadata: [String: String]? = nil
    ) {
        self.responseID = responseID
        self.status = status
        self.statusReason = statusReason
        self.usage = usage
        self.metadata = metadata
    }
}

/// One decoded server event (spec/session.md, `NET-04`).
public enum RealtimeEvent: Sendable, Equatable {
    /// `session.created`: the session type ("realtime" or "transcription") and the model.
    case sessionCreated(sessionType: String, model: String)
    case responseCreated(responseID: String, metadata: [String: String]?)
    /// `response.output_audio.delta`, its base64 PCM decoded.
    case audioDelta(responseID: String, pcm: Data)
    /// `response.output_audio_transcript.delta`: the model's own transcript.
    case transcriptDelta(responseID: String, delta: String)
    case responseDone(ResponseDone)
    case rateLimitsUpdated([RateLimit])
    /// A server `error` event, classified.
    case error(RealtimeError)
    case inputCommitted(itemID: String)
    case transcriptionDelta(itemID: String, delta: String)
    case transcriptionCompleted(itemID: String, transcript: String)
    case transcriptionFailed(itemID: String, error: RealtimeError)
    /// A documented event this package does not consume.
    case ignored(type: String)
    /// A type this package has never seen: API drift. The event's bytes are in the ``RealtimeMessage``.
    case unknown(type: String)

    public static let sessionCreatedType = "session.created"
    public static let responseCreatedType = "response.created"
    public static let audioDeltaType = "response.output_audio.delta"
    public static let transcriptDeltaType = "response.output_audio_transcript.delta"
    public static let responseDoneType = "response.done"
    public static let rateLimitsUpdatedType = "rate_limits.updated"
    public static let errorType = "error"
    public static let inputCommittedType = "input_audio_buffer.committed"
    public static let transcriptionDeltaType = "conversation.item.input_audio_transcription.delta"
    public static let transcriptionCompletedType = "conversation.item.input_audio_transcription.completed"
    public static let transcriptionFailedType = "conversation.item.input_audio_transcription.failed"

    /// Documented events the package does not consume. They are not drift, so they are not counted as unknown.
    public static let ignoredTypes: Set<String> = [
        "session.updated", "response.output_item.added", "response.output_item.done", "response.content_part.added",
        "response.content_part.done", "response.output_audio.done", "response.output_audio_transcript.done",
        "conversation.item.added", "conversation.item.created", "conversation.item.done",
        "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "input_audio_buffer.cleared",
        "conversation.item.input_audio_transcription.segment",
    ]

    /// The event's wire type.
    public var type: String {
        switch self {
        case .sessionCreated: return Self.sessionCreatedType
        case .responseCreated: return Self.responseCreatedType
        case .audioDelta: return Self.audioDeltaType
        case .transcriptDelta: return Self.transcriptDeltaType
        case .responseDone: return Self.responseDoneType
        case .rateLimitsUpdated: return Self.rateLimitsUpdatedType
        case .error: return Self.errorType
        case .inputCommitted: return Self.inputCommittedType
        case .transcriptionDelta: return Self.transcriptionDeltaType
        case .transcriptionCompleted: return Self.transcriptionCompletedType
        case .transcriptionFailed: return Self.transcriptionFailedType
        case .ignored(let type), .unknown(let type): return type
        }
    }

    /// For `rate_limits.updated`: a rate-limited error when any limit has nothing left, else nil.
    public static func exhausted(_ limits: [RateLimit]) -> RealtimeError? {
        guard let spent = limits.first(where: { $0.remaining <= 0 }) else { return nil }
        return RealtimeError(.rateLimited, message: "rate limit \"\(Secret.scrub(spent.name))\" exhausted")
    }

    /// Decodes one server event by its type. Unknown types decode to ``unknown(type:)``. Invalid JSON, a field of the
    /// wrong type, a missing required field, bad base64 or an unknown `response.status` is a `protocol` error that
    /// names the event type only, never the payload. The field rules are Go's `encoding/json` rules for the shapes
    /// both implementations declare, so the two accept and refuse the same events.
    public static func decode(_ data: Data) throws -> RealtimeEvent {
        guard let parsed = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed]) else {
            throw protocolError("", "not valid JSON")
        }
        if parsed is NSNull { throw protocolError("", "missing type") }
        guard let object = parsed as? [String: Any], let w = Wire(object) else {
            throw protocolError("", "not valid JSON")
        }
        guard let type = w.type, !type.isEmpty else { throw protocolError("", "missing type") }
        func missing(_ field: String) -> RealtimeError { protocolError(type, "missing " + field) }
        switch type {
        case sessionCreatedType:
            guard let sessionType = w.sessionType else { throw missing("session.type") }
            return .sessionCreated(sessionType: sessionType, model: w.sessionModel)
        case responseCreatedType:
            guard let r = w.response, let id = r.id else { throw missing("response.id") }
            return .responseCreated(responseID: id, metadata: r.metadata)
        case audioDeltaType:
            guard let id = w.responseID, let delta = w.delta else { throw missing("response_id or delta") }
            guard let pcm = Data(base64Encoded: delta) else { throw protocolError(type, "delta is not base64") }
            return .audioDelta(responseID: id, pcm: pcm)
        case transcriptDeltaType:
            guard let id = w.responseID, let delta = w.delta else { throw missing("response_id or delta") }
            return .transcriptDelta(responseID: id, delta: delta)
        case responseDoneType:
            guard let r = w.response else { throw missing("response") }
            guard let rawStatus = r.status else { throw missing("response.status") }
            guard let id = r.id else { throw missing("response.id") }
            // The value is payload, never named.
            guard let status = ResponseStatus(rawValue: rawStatus) else {
                throw protocolError(type, "unknown response.status")
            }
            return .responseDone(
                ResponseDone(responseID: id, status: status, statusReason: r.statusReason, usage: r.usage, metadata: r.metadata))
        case rateLimitsUpdatedType:
            guard let limits = w.rateLimits else { throw missing("rate_limits") }
            var out: [RateLimit] = []
            for l in limits {
                guard let remaining = l.remaining else { throw missing("rate_limits[].remaining") }
                out.append(RateLimit(name: l.name, limit: l.limit, remaining: remaining, resetSeconds: l.resetSeconds))
            }
            return .rateLimitsUpdated(out)
        case errorType:
            return .error(try classified(type, w.error))
        case inputCommittedType:
            guard let item = w.itemID else { throw missing("item_id") }
            return .inputCommitted(itemID: item)
        case transcriptionDeltaType:
            guard let item = w.itemID, let delta = w.delta else { throw missing("item_id or delta") }
            return .transcriptionDelta(itemID: item, delta: delta)
        case transcriptionCompletedType:
            guard let item = w.itemID, let transcript = w.transcript else { throw missing("item_id or transcript") }
            return .transcriptionCompleted(itemID: item, transcript: transcript)
        case transcriptionFailedType:
            guard let item = w.itemID else { throw missing("item_id") }
            return .transcriptionFailed(itemID: item, error: try classified(type, w.error))
        default:
            return ignoredTypes.contains(type) ? .ignored(type: type) : .unknown(type: type)
        }
    }

    private static func classified(_ type: String, _ e: Wire.ErrorBody?) throws -> RealtimeError {
        guard let e, let errorType = e.type else { throw protocolError(type, "missing error.type") }
        return RealtimeError.errorEvent(type: errorType, code: e.code, param: e.param, message: e.message)
    }

    static func protocolError(_ type: String, _ why: String) -> RealtimeError {
        if type.isEmpty { return RealtimeError(.protocol, message: "realtime event: " + why) }
        return RealtimeError(.protocol, message: "realtime event \(Secret.scrub(type)): \(why)")
    }
}

/// The fields the decoder reads, checked with Go's rules: null is absence, and a present field of the wrong type
/// fails the whole event, whatever its type.
private struct Wire {
    struct ErrorBody {
        var type: String?
        var code = "", message = "", param = ""
    }
    struct Response {
        var id: String?, status: String?
        var statusReason = ""
        var usage = TokenUsage()
        var metadata: [String: String]?
    }
    struct Limit {
        var name = "", limit: Int64 = 0, remaining: Int64?, resetSeconds: Double = 0
    }

    var type: String?, responseID: String?, itemID: String?, delta: String?, transcript: String?
    var response: Response?
    var sessionType: String?
    var sessionModel = ""
    var rateLimits: [Limit]?
    var error: ErrorBody?

    init?(_ o: [String: Any]) {
        guard let type = Self.optional(o["type"]), let responseID = Self.optional(o["response_id"]),
            let itemID = Self.optional(o["item_id"]), let delta = Self.optional(o["delta"]),
            let transcript = Self.optional(o["transcript"])
        else { return nil }
        (self.type, self.responseID, self.itemID, self.delta, self.transcript) =
            (type, responseID, itemID, delta, transcript)

        if let raw = Self.present(o["response"]) {
            guard let r = raw as? [String: Any], let response = Self.response(r) else { return nil }
            self.response = response
        }
        if let raw = Self.present(o["session"]) {
            guard let s = raw as? [String: Any], let st = Self.optional(s["type"]), let model = JSONField.string(s["model"])
            else { return nil }
            (sessionType, sessionModel) = (st, model)
        }
        if let raw = Self.present(o["rate_limits"]) {
            guard let list = raw as? [Any] else { return nil }
            var limits: [Limit] = []
            for item in list {
                var l = Limit()
                if let entry = Self.present(item) {
                    guard let e = entry as? [String: Any], let name = JSONField.string(e["name"]),
                        let limit = JSONField.int(e["limit"]), let remaining = Self.optionalInt(e["remaining"]),
                        let reset = JSONField.double(e["reset_seconds"])
                    else { return nil }
                    (l.name, l.limit, l.remaining, l.resetSeconds) = (name, limit, remaining, reset)
                }
                limits.append(l)
            }
            rateLimits = limits
        }
        if let raw = Self.present(o["error"]) {
            guard let e = raw as? [String: Any], let errorType = Self.optional(e["type"]),
                let message = JSONField.string(e["message"]), let param = JSONField.string(e["param"])
            else { return nil }
            error = ErrorBody(type: errorType, code: JSONField.rawString(e["code"]), message: message, param: param)
        }
    }

    private static func response(_ r: [String: Any]) -> Response? {
        guard let id = optional(r["id"]), let status = optional(r["status"]) else { return nil }
        var out = Response(id: id, status: status)
        if let raw = present(r["status_details"]) {
            guard let d = raw as? [String: Any], let reason = JSONField.string(d["reason"]) else { return nil }
            out.statusReason = reason
        }
        if let raw = present(r["usage"]) {
            guard let u = raw as? [String: Any], let input = JSONField.int(u["input_tokens"]),
                let output = JSONField.int(u["output_tokens"])
            else { return nil }
            out.usage.inputTokens = Int(input)
            out.usage.outputTokens = Int(output)
            if let rawIn = present(u["input_token_details"]) {
                guard let d = rawIn as? [String: Any], let text = JSONField.int(d["text_tokens"]) else { return nil }
                out.usage.inputTextTokens = Int(text)
            }
            if let rawOut = present(u["output_token_details"]) {
                guard let d = rawOut as? [String: Any], let text = JSONField.int(d["text_tokens"]),
                    let audio = JSONField.int(d["audio_tokens"])
                else { return nil }
                (out.usage.outputTextTokens, out.usage.outputAudioTokens, out.usage.detailed) = (Int(text), Int(audio), true)
            }
        }
        if let raw = present(r["metadata"]) {
            guard let m = raw as? [String: Any] else { return nil }
            var metadata: [String: String] = [:]
            for (k, v) in m {
                guard let s = JSONField.string(v) else { return nil }
                metadata[k] = s
            }
            out.metadata = metadata
        }
        return out
    }

    /// The value unless it is absent or null.
    private static func present(_ v: Any?) -> Any? {
        guard let v, !(v is NSNull) else { return nil }
        return v
    }

    /// `.some(nil)` for absence or null, `.some(s)` for a string, nil for any other type.
    private static func optional(_ v: Any?) -> String?? {
        guard let v = present(v) else { return .some(nil) }
        guard let s = v as? String else { return nil }
        return .some(s)
    }

    private static func optionalInt(_ v: Any?) -> Int64?? {
        guard present(v) != nil else { return .some(nil) }
        guard let n = JSONField.int(v) else { return nil }
        return .some(n)
    }
}

/// One message as received: its bytes and its event, decoded once. Unknown and ignored events are delivered too, so
/// an app can read what this package does not consume (a tool call, say) from ``raw``.
public struct RealtimeMessage: Sendable, Equatable {
    public let raw: Data
    public let event: RealtimeEvent

    public init(raw: Data, event: RealtimeEvent) {
        self.raw = raw
        self.event = event
    }

    /// Decodes `raw`; see ``RealtimeEvent/decode(_:)``.
    public init(raw: Data) throws {
        self.raw = raw
        self.event = try RealtimeEvent.decode(raw)
    }
}
