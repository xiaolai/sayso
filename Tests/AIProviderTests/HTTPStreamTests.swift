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
}
