import Foundation

/// **One answer put together from a stream of chat-completions chunks**, never a partial one: only a turn the server
/// says it finished is an answer, read by the same rules as a whole reply's `finish_reason`.
///
/// **Only `content` builds the answer**: an allowlist, so reasoning deltas (`reasoning_content`, `reasoning`,
/// `reasoning_details`) and any key a server adds later are parsed and dropped, never kept. Only choice `0` is read: no
/// request asks for more. The answer is capped as it grows, at `cap` UTF-8 bytes.
struct StreamedAnswer: Sendable {
    /// The HTTP status the stream came with, for an error the server sends inside it.
    private let status: Int
    private let cap: Int
    private let key: String?
    private var content = ""
    private var refused = false
    /// The first finish reason choice `0` gave; `nil` while the turn is unfinished.
    private var finish: String?

    init(status: Int, cap: Int, key: String?) {
        self.status = status
        self.cap = cap
        self.key = key
    }

    /// Takes one event's data; `true` once the answer has ended, so nothing after it need be read. A server's error
    /// event is `.server` with its own words, scrubbed of the key; anything that is not a chunk is `.badShape`.
    mutating func take(_ event: Data) throws(AIProviderError) -> Bool {
        // **Each event's Foundation objects go with it**: parsing autoreleases them, and a stream that outruns its
        // reader never lets the read loop pause to drain them, so they would pile up for as long as the stream runs.
        #if canImport(ObjectiveC)
        try autoreleasepool { Result { () throws(AIProviderError) in try read(event) } }.get()
        #else
        try read(event)
        #endif
    }

    private mutating func read(_ event: Data) throws(AIProviderError) -> Bool {
        if String(decoding: event, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) == "[DONE]" {
            return true
        }
        guard let object = try? JSONSerialization.jsonObject(with: event) as? [String: Any] else { throw .badShape }
        if object["error"] is [String: Any] || object["error"] is String {
            throw .server(status: status, message: HTTPProvider.message(in: event, key: key))
        }
        guard let choices = object["choices"] else { return false }
        guard let choices = choices as? [[String: Any]] else { throw .badShape }
        for choice in choices where choice["index"] as? Int ?? 0 == 0 {
            if let delta = choice["delta"] as? [String: Any] {
                if let text = delta["content"] as? String {
                    guard content.utf8.count + text.utf8.count <= cap else { throw .badShape }
                    content += text
                } else if let other = delta["content"], !(other is NSNull) {
                    throw .badShape
                }
                if let refusal = delta["refusal"] as? String, !refusal.isEmpty { refused = true }
            }
            if let reason = choice["finish_reason"], !(reason is NSNull) {
                // A reason that is not a string is an unknown one: no answer.
                finish = reason as? String ?? ""
                return true
            }
        }
        return false
    }

    /// The answer, once the stream has ended; `allowEmpty` and `allowLength` are the probe's, as for a whole reply.
    func text(allowEmpty: Bool = false, allowLength: Bool = false) throws(AIProviderError) -> String {
        switch finish {
        case "stop": break
        case "length": if !allowLength { throw .truncated }
        case "content_filter": throw .refused
        default: throw .badShape
        }
        if refused { throw .refused }
        if content.isEmpty, !allowEmpty { throw .badShape }
        return content
    }
}
