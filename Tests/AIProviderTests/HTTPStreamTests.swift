import Foundation
import Testing
@testable import SaySoAI

/// **A streamed answer is framed and put together from bytes alone**: the WHATWG event-stream rules applied byte by
/// byte, then the chat-completions chunks read into one answer or one typed failure. Pure, no network: each input is a
/// byte string and an expected outcome, so it can become a fixture later without being rewritten.
struct HTTPStreamTests {
    // MARK: - Framing

    /// Every event the bytes dispatch, the end of the stream included, fed in the given pieces.
    static func events(_ pieces: [String], cap: Int = 1 << 20) throws(WireFailure) -> [String] {
        var framer = ServerSentEvents(cap: cap)
        var events: [String] = []
        for piece in pieces {
            for byte in Data(piece.utf8) {
                if let event = try framer.take(byte) { events.append(String(decoding: event, as: UTF8.self)) }
            }
        }
        if let event = try framer.finish() { events.append(String(decoding: event, as: UTF8.self)) }
        return events
    }

    @Test func eachLineEndEndsALine() throws {
        #expect(try Self.events(["data: a\n\ndata: b\r\n\r\ndata: c\r\rdata: d\n\r\n"]) == ["a", "b", "c", "d"])
    }

    /// **A CR at the end of one piece and the LF that starts the next are one line end**, not a line and a blank line
    /// that would dispatch an event early.
    @Test func aCRAndLFSplitAcrossPiecesAreOneLineEnd() throws {
        #expect(try Self.events(["data: a\r", "\ndata: b\r", "\n\r", "\n"]) == ["a\nb"])
    }

    @Test func dataLinesJoinWithLineFeeds() throws {
        #expect(try Self.events(["data: one\ndata:two\ndata:  three\n\n"]) == ["one\ntwo\n three"])
    }

    @Test func commentsAndOtherFieldsAreIgnored() throws {
        let stream = ": keep-alive\nevent: message\nid: 7\nretry: 100\nmystery: x\nnocolon\ndata: a\n: between\n\n"
        #expect(try Self.events([stream]) == ["a"])
        #expect(try Self.events([": only a comment\n\n"]) == [])
    }

    @Test func aLeadingByteOrderMarkIsSkippedOnlyAtTheStart() throws {
        #expect(try Self.events(["\u{FEFF}data: a\n\n"]) == ["a"])
        // Mid-stream it is data like any other character.
        #expect(try Self.events(["data: a\n\ndata: \u{FEFF}b\n\n"]) == ["a", "\u{FEFF}b"])
        // A stream that only starts like a mark keeps its bytes.
        #expect(try Self.events(["\u{FEFF}\u{FEFF}data: a\n\n"]) == [])
    }

    /// **A separator that is not CR or LF is plain data**: U+2028, U+2029 and U+0085 may sit unescaped inside a JSON
    /// string, and a line reader that splits on them breaks one data line in two.
    @Test func unicodeSeparatorsInsideDataAreData() throws {
        #expect(try Self.events(["data: x\u{2028}y\u{2029}z\u{85}w\n\n"]) == ["x\u{2028}y\u{2029}z\u{85}w"])
    }

    @Test func anEventWithEmptyDataIsIgnored() throws {
        #expect(try Self.events(["data:\n\ndata: \n\n\n\ndata: a\n\n"]) == ["a"])
    }

    /// More lenient than WHATWG, which drops it: a cut JSON object still fails to parse, so this cannot turn a broken
    /// stream into an answer.
    @Test func theLastEventIsDispatchedAtTheEnd() throws {
        #expect(try Self.events(["data: a\n\ndata: b"]) == ["a", "b"])
        #expect(try Self.events(["data: a\n\ndata: b\n"]) == ["a", "b"])
        #expect(try Self.events(["data: a\r"]) == ["a"])
    }

    /// **The caps hold as the bytes arrive**: a line or an event past the cap fails at the byte that passes it.
    @Test func aLineOrAnEventPastTheCapIsTooLarge() throws {
        let line = "data: " + String(repeating: "a", count: 10)
        #expect(try Self.events([line + "\n\n"], cap: line.utf8.count) == [String(repeating: "a", count: 10)])
        #expect(throws: WireFailure.tooLarge) { try Self.events([line + "\n\n"], cap: line.utf8.count - 1) }
        #expect(throws: WireFailure.tooLarge) { try Self.events([line], cap: line.utf8.count - 1) }
        #expect(throws: WireFailure.tooLarge) { try Self.events([": " + String(repeating: "c", count: 10)], cap: 8) }
        // Each line fits; their joined data does not.
        #expect(try Self.events(["data: aaaa\ndata: bbb\n\n"], cap: 10) == ["aaaa\nbbb"])
        #expect(throws: WireFailure.tooLarge) { try Self.events(["data:aaaaa\ndata:bbbbb\n\n"], cap: 10) }
    }

    // MARK: - Putting the answer together

    /// One chat-completions chunk: a choice with `delta`, and `finish_reason` when given.
    static func chunk(_ delta: [String: Any] = [:], finish: String? = nil, index: Int? = 0) -> String {
        var choice: [String: Any] = ["delta": delta, "finish_reason": finish.map { $0 as Any } ?? NSNull()]
        if let index { choice["index"] = index }
        return json(["id": "c1", "object": "chat.completion.chunk", "choices": [choice]])
    }

    static func json(_ object: [String: Any]) -> String {
        guard let data = try? JSONSerialization.data(withJSONObject: object) else { return "{}" }
        return String(decoding: data, as: UTF8.self)
    }

    /// The events in order until one ends the answer, then the answer an ask or a probe reads from them.
    static func assemble(_ events: [String], cap: Int = 1 << 20, probe: Bool = false) throws(AIProviderError) -> String {
        var answer = StreamedAnswer(status: 200, cap: cap, key: "sk-SECRET-123")
        for event in events {
            if try answer.take(Data(event.utf8)) { break }
        }
        return try answer.text(allowEmpty: probe, allowLength: probe)
    }

    @Test func contentDeltasMakeTheAnswer() throws {
        let events = [Self.chunk(["role": "assistant", "content": ""]), Self.chunk(["content": "Hel"]),
                      Self.chunk(["content": NSNull()]), Self.chunk([:]), Self.chunk(["content": "lo"], finish: "stop")]
        #expect(try Self.assemble(events) == "Hello")
        #expect(try Self.assemble(events, probe: true) == "Hello")
    }

    /// **Only `content` builds the answer**: an allowlist, not a list of reasoning fields to drop, so a server's new
    /// reasoning key cannot leak into the reader's answer.
    @Test func everyOtherDeltaKeyIsIgnored() throws {
        let events = [Self.chunk(["reasoning_content": "think A"]), Self.chunk(["reasoning": "think B"]),
                      Self.chunk(["reasoning_details": [["type": "text", "text": "think C"]]]),
                      Self.chunk(["tool_calls": [["index": 0]], "mystery": "D"]),
                      Self.chunk(["content": "answer"], finish: "stop")]
        #expect(try Self.assemble(events) == "answer")
    }

    /// Kept exactly as the non-streamed path keeps it: the caller validates.
    @Test func thinkingWrittenInsideContentIsKept() throws {
        #expect(try Self.assemble([Self.chunk(["content": "<think>x</think>y"], finish: "stop")]) == "<think>x</think>y")
    }

    @Test func aUsageOnlyChunkAndAnotherChoiceAreIgnored() throws {
        let events = [Self.chunk(["content": "a"], index: nil), Self.chunk(["content": "OTHER"], finish: "stop", index: 1),
                      Self.json(["choices": [], "usage": ["completion_tokens": 3]]), Self.json(["usage": [:]]),
                      Self.chunk(["content": "b"], finish: "stop")]
        #expect(try Self.assemble(events) == "ab")
    }

    /// **Only the integer 0, or no `index` at all, is the answer's choice**: an index that only looks like 0 once
    /// converted (a string, a boolean, a fraction, null) is another choice, so its content and finish are ignored.
    @Test func anIndexThatIsNotTheIntegerZeroIsAnotherChoice() throws {
        // Written as JSON text: serializing a Swift `0.0` would write `0`.
        let others = [#""1""#, #""0""#, "false", "true", "0.5", "0.0", "null"]
        let events = others.map { #"{"choices":[{"index":\#($0),"delta":{"content":"OTHER"},"finish_reason":"stop"}]}"# }
            + [Self.chunk(["content": "a"], index: nil), Self.chunk(["content": "b"], finish: "stop")]
        #expect(try Self.assemble(events) == "ab")
    }

    /// **The first finish reason ends the answer**: whatever comes after it is never read into it.
    @Test func theFirstFinishReasonEndsTheAnswer() throws {
        var answer = StreamedAnswer(status: 200, cap: 1 << 20, key: nil)
        #expect(try answer.take(Data(Self.chunk(["content": "a"]).utf8)) == false)
        #expect(try answer.take(Data(Self.chunk(["content": "b"], finish: "stop").utf8)))
        #expect(try Self.assemble([Self.chunk(["content": "a"], finish: "stop"), Self.chunk(["content": "b"])]) == "a")
    }

    @Test func howItEndsDecidesTheOutcome() throws {
        let endings: [(String, String, AIProviderError, AIProviderError?)] = [
            ("stop", "", .badShape, nil),
            ("length", "half", .truncated, nil),
            ("content_filter", "", .refused, .refused),
            ("tool_calls", "x", .badShape, .badShape),
            ("function_call", "x", .badShape, .badShape),
            ("error", "x", .badShape, .badShape),
            ("mystery", "x", .badShape, .badShape),
        ]
        for (finish, content, ask, probe) in endings {
            let events = [Self.chunk(["content": content], finish: finish)]
            #expect(throws: ask, "finish_reason \(finish)") { try Self.assemble(events) }
            if let probe {
                #expect(throws: probe, "probe, finish_reason \(finish)") { try Self.assemble(events, probe: true) }
            } else {
                #expect(try Self.assemble(events, probe: true) == content)
            }
        }
    }

    @Test func aRefusalIsRefused() throws {
        let events = [Self.chunk(["refusal": "I can't help with that."]), Self.chunk([:], finish: "stop")]
        #expect(throws: AIProviderError.refused) { try Self.assemble(events) }
        #expect(throws: AIProviderError.refused) { try Self.assemble(events, probe: true) }
        #expect(try Self.assemble([Self.chunk(["refusal": "", "content": "ok"], finish: "stop")]) == "ok")
    }

    /// **A stream that never says why it ended is no answer**: neither `[DONE]` nor the end of the bytes is a finish
    /// reason, and the content before them is partial.
    @Test func anEndWithoutAFinishReasonIsABadShape() {
        let partial = [Self.chunk(["content": "half"])]
        for events in [partial + ["[DONE]"], partial + [" [DONE] "], partial] {
            #expect(throws: AIProviderError.badShape) { try Self.assemble(events) }
            #expect(throws: AIProviderError.badShape) { try Self.assemble(events, probe: true) }
        }
    }

    /// **A server's error mid-stream is its own words**, at the stream's status, scrubbed of the key.
    @Test func anErrorEventCarriesTheServersWordsWithoutTheKey() {
        for event in [#"{"error":{"message":"overloaded for sk-SECRET-123","type":"server_error"}}"#,
                      #"{"error":"overloaded for sk-SECRET-123"}"#] {
            #expect(throws: AIProviderError.server(status: 200, message: "overloaded for …")) {
                try Self.assemble([Self.chunk(["content": "a"]), event])
            }
        }
    }

    @Test func anEventThatIsNotAChunkIsABadShape() {
        for event in ["not json", #"["a"]"#, #"{"choices":"x"}"#, Self.chunk(["content": 7], finish: "stop")] {
            #expect(throws: AIProviderError.badShape, "\(event)") { try Self.assemble([event]) }
        }
    }

    /// **Reading events holds no memory per event**: a stream that outruns its reader never lets the read loop pause,
    /// so whatever parsing an event leaves for a later cleanup piles up. What it leaves is about the event's own size,
    /// so large events make each one count: unbounded, 4,000 events of 16 KB held about 64 MB here; the ceiling is far
    /// under that and far over what one event leaves.
    @Test(.timeLimit(.minutes(1))) func readingEventsHoldsNoMemoryPerEvent() throws {
        // The process's physical footprint: malloc's own statistics read zero under the thread sanitizer's allocator.
        func inUse() -> Int {
            var info = task_vm_info_data_t()
            var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<natural_t>.size)
            let result = withUnsafeMutablePointer(to: &info) {
                $0.withMemoryRebound(to: integer_t.self, capacity: Int(count)) {
                    task_info(mach_task_self_, task_flavor_t(TASK_VM_INFO), $0, &count)
                }
            }
            return result == KERN_SUCCESS ? Int(info.phys_footprint) : 0
        }
        // The instrument must see memory being used before its silence means anything. Fresh pages from `mmap`, not
        // malloc, which may hand back freed pages already counted; half of them is enough, as other tests running
        // meanwhile may free some of theirs.
        let size = 16 << 20
        let before = inUse()
        let probe = try #require(mmap(nil, size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0))
        try #require(probe != MAP_FAILED)
        probe.initializeMemory(as: UInt8.self, repeating: 1, count: size)
        let seen = inUse() - before
        munmap(probe, size)
        try #require(seen >= size / 2, "the footprint did not show a 16 MB mapping: \(seen) bytes")

        var answer = StreamedAnswer(status: 200, cap: 1 << 20, key: nil)
        let event = Data(Self.json(["choices": [], "pad": String(repeating: "x", count: 16_000)]).utf8)
        let start = inUse()
        var peak = start
        for count in 1...4_000 {
            _ = try answer.take(event)
            if count % 100 == 0 { peak = max(peak, inUse()) }
        }
        #expect(peak - start < 16 << 20, "4,000 events held \(peak - start) bytes")
    }

    /// **The answer is capped as it grows**: its UTF-8 bytes up to the cap are an answer, one more is not.
    @Test func contentPastTheCapIsABadShape() throws {
        let events = [Self.chunk(["content": "é"]), Self.chunk(["content": "abc"], finish: "stop")]
        #expect(try Self.assemble(events, cap: 5) == "éabc")
        #expect(throws: AIProviderError.badShape) { try Self.assemble(events, cap: 4) }
    }
}
