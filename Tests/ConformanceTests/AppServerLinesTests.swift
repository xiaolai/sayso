#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import XCTest

/// The app-server reader's line buffer, as go/codexapp/lines_test.go: the lines it splits, and that it leaves nothing
/// of a line in memory it freed or moved away from.
final class AppServerLinesTests: XCTestCase {
    /// Hands `stream` to a reader in reads of the sizes `chunks` cycles through.
    private func reader(_ stream: UnsafeRawBufferPointer, _ chunks: [Int]) -> LineReader {
        var at = 0, k = 0
        return LineReader { buf in
            guard at < stream.count else { return nil }
            let n = min(chunks[k % chunks.count], buf.count, stream.count - at)
            k += 1
            buf.baseAddress!.copyMemory(from: stream.baseAddress! + at, byteCount: n)
            at += n
            return n
        }
    }

    private func lines(_ text: String, _ chunks: [Int]) -> [String] {
        let bytes = Array(text.utf8)
        return bytes.withUnsafeBytes { raw in
            let r = reader(raw, chunks)
            var out: [String] = []
            while true {
                switch r.next() {
                case .line(let l, let last): out.append(String(decoding: l, as: UTF8.self) + (last ? "<eof>" : ""))
                case .tooLong: return out + ["<too long>"]
                case .ended: return out
                }
            }
        }
    }

    /// Lines end at a newline, a last line at the end of the stream, whatever the reads; an empty line is a line.
    func testLinesAreSplitAsTheyCome() {
        let big = String(repeating: "y", count: 200_000)
        let text = "a\n\nbc\r\n" + big + "\nlast"
        let want = ["a", "", "bc\r", big, "last<eof>"]
        for chunks in [[1], [3, 7], [65_536], [100_000, 1], [1 << 20]] {
            XCTAssertEqual(lines(text, chunks), want, "reads of \(chunks)")
        }
        XCTAssertEqual(lines("", [5]), [])
        XCTAssertEqual(lines("x\n", [5]), ["x"])
    }

    /// A line of exactly 4 MiB is a line; one byte more before the newline is too long, as is an unended one.
    func testALineAtTheLimitFitsAndOneMoreIsTooLong() {
        let limit = AppServerClient.maxLine
        XCTAssertEqual(lines(String(repeating: "x", count: limit) + "\nz", [65_536]).map(\.count), [limit, 6])
        XCTAssertEqual(lines(String(repeating: "x", count: limit + 1) + "\nz", [65_536]), ["<too long>"])
        XCTAssertEqual(lines("a\n" + String(repeating: "x", count: limit + 1), [1 << 20]), ["a", "<too long>"])
    }

    /// Nothing of a line is left in a buffer the reader outgrew, in the bytes left behind when the unread ones move
    /// to the front, or in what it drains; the line itself is the caller's to wipe.
    func testTheReaderLeavesNothing() throws {
        if MemoryScan.allocatorReplaced { throw XCTSkip("a sanitizer replaced the allocator the scan watches") }
        // A line outgrows the first buffer, its needle read before it does.
        var w = WipeScan(room: 1 << 20)
        var doc = w.emit([.kept, .text(String(repeating: "x", count: 200_000) + "\n")])
        var r = reader(doc, [4096])
        var line: UnsafeMutableRawBufferPointer?
        w.check(w.found { if case .line(let l, _) = r.next() { line = l } })
        XCTAssertEqual(line?.count, doc.count - 1)
        XCTAssertEqual(w.found { wipe(line!) }, [0, 0])

        // A line whose needle is read before the bytes in front of it are consumed moves to the front.
        w = WipeScan(room: 1 << 18)
        doc = w.emit([.text(String(repeating: "x", count: 65_000) + "\n"), .kept, .text(String(repeating: "y", count: 2000) + "\n")])
        r = reader(doc, [65_500, 4096])
        guard case .line(let first, _) = r.next() else { return XCTFail("no first line") }
        XCTAssertEqual(first.count, 65_000)
        w.check(w.found { if case .line(let l, _) = r.next() { line = l } })
        XCTAssertEqual(line?.count, 2040)
        r.wipe()
        XCTAssertEqual(w.found {}, [0, 0], "the reader's wipe left the line")

        // What is drained after a wipe is wiped read by read: the last read, with its needle, is left in the buffer.
        w = WipeScan()
        doc = w.emit([.text("a\n" + String(repeating: "x", count: 200)), .needle, .text("\n"), .needle])
        r = reader(doc, [2, 1000])
        _ = r.next()
        r.wipe()
        w.check(w.found { r.drain() })
    }
}
#endif
