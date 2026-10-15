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
        return RealtimeError(.rateLimited, message: "rate limit \(goQuoted(Secret.scrub(spent.name))) exhausted")
    }

    /// Decodes one server event by its type. The whole event must be strict JSON (spec/session.md, wire JSON); then
    /// only the fields of its type are read, so an ignored or unknown type is never refused for the shape of a field
    /// this package does not consume. A field of the wrong type, a missing required field, bad base64 or an unknown
    /// `response.status` is a `protocol` error that names the event type only, never the payload.
    public static func decode(_ data: Data) throws -> RealtimeEvent {
        let notJSON = protocolError("", "not valid JSON")
        guard let root = try? WireJSON.parse(data) else { throw notJSON }
        if root.isNull { throw protocolError("", "missing type") }
        // As in Go: not an object, or a type that is not a string, is not valid JSON; an absent, null or empty type is
        // a missing type.
        guard let w = try? WireJSON.Object(root) else { throw notJSON }
        let type: String?
        do { type = try w.string("type") } catch { throw notJSON }
        guard let type, !type.isEmpty else { throw protocolError("", "missing type") }
        do {
            if let event = try typed(type, w) { return event }
        } catch is WireJSON.Invalid {
            throw protocolError(type, "a field has the wrong type")  // the type is known: name it
        }
        return ignoredTypes.contains(type) ? .ignored(type: type) : .unknown(type: type)
    }

    /// The consumed types; nil for any other. Every field of the type is read before a missing one is reported, as
    /// in Go.
    private static func typed(_ type: String, _ w: WireJSON.Object) throws -> RealtimeEvent? {
        func missing(_ field: String) -> RealtimeError { protocolError(type, "missing " + field) }
        switch type {
        case sessionCreatedType:
            guard let session = try w.object("session") else { throw missing("session.type") }
            let sessionType = try session.string("type")
            let model = try session.string("model") ?? ""
            guard let sessionType else { throw missing("session.type") }
            return .sessionCreated(sessionType: sessionType, model: model)
        case responseCreatedType:
            guard let r = try w.object("response") else { throw missing("response.id") }
            let id = try r.string("id")
            let metadata = try r.stringMap("metadata")
            guard let id else { throw missing("response.id") }
            return .responseCreated(responseID: id, metadata: metadata)
        case audioDeltaType:
            let (id, delta) = (try w.string("response_id"), try w.string("delta"))
            guard let id, let delta else { throw missing("response_id or delta") }
            guard let pcm = StrictBase64.decode(delta) else { throw protocolError(type, "delta is not base64") }
            return .audioDelta(responseID: id, pcm: pcm)
        case transcriptDeltaType:
            let (id, delta) = (try w.string("response_id"), try w.string("delta"))
            guard let id, let delta else { throw missing("response_id or delta") }
            return .transcriptDelta(responseID: id, delta: delta)
        case responseDoneType:
            return .responseDone(try done(type, w))
        case rateLimitsUpdatedType:
            guard let list = try w.array("rate_limits") else { throw missing("rate_limits") }
            var limits: [RateLimit] = []
            for item in list {
                var remaining: Int64?
                var l = RateLimit(name: "", limit: 0, remaining: 0, resetSeconds: 0)
                if !item.isNull {
                    let o = try WireJSON.Object(item)
                    l.name = try o.string("name") ?? ""
                    l.limit = try o.int("limit") ?? 0
                    remaining = try o.int("remaining")
                    l.resetSeconds = try o.double("reset_seconds") ?? 0
                }
                guard let remaining else { throw missing("rate_limits[].remaining") }
                l.remaining = remaining
                limits.append(l)
            }
            return .rateLimitsUpdated(limits)
        case errorType:
            return .error(try classified(type, w))
        case inputCommittedType:
            guard let item = try w.string("item_id") else { throw missing("item_id") }
            return .inputCommitted(itemID: item)
        case transcriptionDeltaType:
            let (item, delta) = (try w.string("item_id"), try w.string("delta"))
            guard let item, let delta else { throw missing("item_id or delta") }
            return .transcriptionDelta(itemID: item, delta: delta)
        case transcriptionCompletedType:
            let (item, transcript) = (try w.string("item_id"), try w.string("transcript"))
            guard let item, let transcript else { throw missing("item_id or transcript") }
            return .transcriptionCompleted(itemID: item, transcript: transcript)
        case transcriptionFailedType:
            let item = try w.string("item_id")
            let classifiedError: Result<RealtimeError, Error>
            do { classifiedError = .success(try classified(type, w)) } catch let invalid as WireJSON.Invalid {
                throw invalid
            } catch { classifiedError = .failure(error) }
            guard let item else { throw missing("item_id") }
            return .transcriptionFailed(itemID: item, error: try classifiedError.get())
        default:
            return nil
        }
    }

    private static func done(_ type: String, _ w: WireJSON.Object) throws -> ResponseDone {
        guard let r = try w.object("response") else { throw protocolError(type, "missing response") }
        let id = try r.string("id")
        let rawStatus = try r.string("status")
        let reason = try r.object("status_details")?.string("reason") ?? ""
        var usage = TokenUsage()
        if let u = try r.object("usage") {
            usage.inputTokens = try count(u, "input_tokens")
            usage.outputTokens = try count(u, "output_tokens")
            if let d = try u.object("input_token_details") { usage.inputTextTokens = try count(d, "text_tokens") }
            if let d = try u.object("output_token_details") {
                usage.outputTextTokens = try count(d, "text_tokens")
                usage.outputAudioTokens = try count(d, "audio_tokens")
                usage.detailed = true
            }
        }
        let metadata = try r.stringMap("metadata")
        guard let rawStatus else { throw protocolError(type, "missing response.status") }
        guard let id else { throw protocolError(type, "missing response.id") }
        // The value is payload, never named.
        guard let status = ResponseStatus(rawValue: rawStatus) else {
            throw protocolError(type, "unknown response.status")
        }
        return ResponseDone(responseID: id, status: status, statusReason: reason, usage: usage, metadata: metadata)
    }

    /// A token count; one that does not fit `Int` is invalid, as in Go, rather than a trap.
    private static func count(_ o: WireJSON.Object, _ name: String) throws -> Int {
        guard let v = Int(exactly: try o.int(name) ?? 0) else { throw WireJSON.Invalid() }
        return v
    }

    /// The event's `error` object, classified; a missing error or `error.type` is a protocol error.
    private static func classified(_ type: String, _ w: WireJSON.Object) throws -> RealtimeError {
        var errorType: String?
        var message = "", param = "", code = ""
        if let e = try w.object("error") {
            errorType = try e.string("type")
            message = try e.string("message") ?? ""
            param = try e.string("param") ?? ""
            code = try e.code("code")
        }
        guard let errorType else { throw protocolError(type, "missing error.type") }
        return RealtimeError.errorEvent(type: errorType, code: code, param: param, message: message)
    }

    static func protocolError(_ type: String, _ why: String) -> RealtimeError {
        if type.isEmpty { return RealtimeError(.protocol, message: "realtime event: " + why) }
        return RealtimeError(.protocol, message: "realtime event \(Secret.scrub(type)): \(why)")
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

/// `s` quoted as Go's `%q` (strconv.Quote) does, so a name cannot forge a line in a log and both implementations write
/// the same text: `"` and `\` escaped, `\a \b \f \n \r \t \v` by name, any other character Go does not print as
/// `\xHH` below U+0080, `\uHHHH` below U+10000, else `\UHHHHHHHH`.
func goQuoted(_ s: String) -> String {
    var out = "\""
    for scalar in s.unicodeScalars {
        switch scalar {
        case "\"": out += "\\\""
        case "\\": out += "\\\\"
        case "\u{07}": out += "\\a"
        case "\u{08}": out += "\\b"
        case "\u{0C}": out += "\\f"
        case "\n": out += "\\n"
        case "\r": out += "\\r"
        case "\t": out += "\\t"
        case "\u{0B}": out += "\\v"
        default:
            if goPrintable(scalar) {
                out.unicodeScalars.append(scalar)
            } else if scalar.value < 0x80 {
                out += "\\x" + hex(scalar.value, 2)
            } else if scalar.value < 0x10000 {
                out += "\\u" + hex(scalar.value, 4)
            } else {
                out += "\\U" + hex(scalar.value, 8)
            }
        }
    }
    return out + "\""
}

/// Go's strconv.IsPrint: letters, marks, numbers, punctuation, symbols and the ASCII space.
private func goPrintable(_ s: Unicode.Scalar) -> Bool {
    if s == " " { return true }
    switch s.properties.generalCategory {
    case .uppercaseLetter, .lowercaseLetter, .titlecaseLetter, .modifierLetter, .otherLetter,
        .nonspacingMark, .spacingMark, .enclosingMark,
        .decimalNumber, .letterNumber, .otherNumber,
        .connectorPunctuation, .dashPunctuation, .openPunctuation, .closePunctuation, .initialPunctuation,
        .finalPunctuation, .otherPunctuation,
        .mathSymbol, .currencySymbol, .modifierSymbol, .otherSymbol:
        return true
    default:
        return false
    }
}

private func hex(_ v: UInt32, _ width: Int) -> String {
    let h = String(v, radix: 16)
    return String(repeating: "0", count: max(0, width - h.count)) + h
}
