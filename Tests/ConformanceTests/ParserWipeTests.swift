import Foundation
import XCTest

@testable import RealtimeConnection

/// The bytes the parser reads a secret from are wiped where they live, not in a copy.
final class ParserWipeTests: XCTestCase {
    /// Records whether a buffer held only zeros when it was freed.
    private final class Freed: @unchecked Sendable {
        private let lock = NSLock()
        private var zeroed: [Bool] = []
        func record(_ z: Bool) { lock.withLock { zeroed.append(z) } }
        var results: [Bool] { lock.withLock { zeroed } }
    }

    /// A reply whose body is freed through `freed`, so a test sees the storage the minter received, not a copy.
    private func reply(_ text: String, _ freed: Freed) -> HTTPReply? {
        let bytes = Array(text.utf8)
        let storage = UnsafeMutableRawPointer.allocate(byteCount: bytes.count, alignment: 1)
        storage.copyMemory(from: bytes, byteCount: bytes.count)
        let body = Data(bytesNoCopy: storage, count: bytes.count, deallocator: .custom { p, n in
            freed.record(!UnsafeRawBufferPointer(start: p, count: n).contains { $0 != 0 })
            p.deallocate()
        })
        return HTTPReply(status: 200, headers: ["x-request-id": "req_wipe"], body: body)
    }

    func testMintReplyIsWipedInPlaceOnEveryExit() throws {
        let replies = [
            #"{"value":"ek_wipe_canary","expires_at":1800000600}"#,
            #"{"value":"ek_wipe_canary","expires_at":1800000600"#,
            #"{"value":"ek_wipe_canary"}"#,
            #"{"value":"","expires_at":1800000600}"#,
        ]
        for (i, text) in replies.enumerated() {
            let freed = Freed()
            var r = reply(text, freed)
            let secret = try? Minter.secret(from: &r)
            XCTAssertNil(r, "\(i): the reply is taken")
            XCTAssertEqual(secret != nil, i == 0, "\(i)")
            XCTAssertEqual(freed.results, [true], "\(i): the body was freed unwiped, or not freed")
        }
    }

    /// The wipe reports where it wrote, so a document can tell an in-place wipe from one that cleared a copy.
    func testWipeTellsAnInPlaceWipeFromACopy() {
        var alone = [UInt8](repeating: 7, count: 16)
        let at = alone.withUnsafeBytes { UInt(bitPattern: $0.baseAddress) }
        XCTAssertEqual(WireJSON.wipe(&alone), at)
        XCTAssertEqual(alone, [UInt8](repeating: 0, count: 16))

        var shared = [UInt8](repeating: 7, count: 16)
        let other = shared
        let sharedAt = other.withUnsafeBytes { UInt(bitPattern: $0.baseAddress) }
        XCTAssertNotEqual(WireJSON.wipe(&shared), sharedAt)
        XCTAssertEqual(other, [UInt8](repeating: 7, count: 16), "the shared original is left as it was")
    }

    /// Every way to read a document ends with it released; a document asserts on release that it was wiped in place,
    /// so a debug run of this test fails when a reader keeps a copy or the wipe is skipped.
    func testParsedDocumentsAreWipedWhenReleased() throws {
        let doc = Data(#"{"type":"x","params":{"token":"ek_doc_canary"},"n":[1,2]}"#.utf8)
        XCTAssertEqual(try WireJSON.memberBytes(doc, "params"), Data(#"{"token":"ek_doc_canary"}"#.utf8))
        XCTAssertEqual(try WireJSON.memberBytes(ArraySlice(doc), "absent"), nil)
        XCTAssertThrowsError(try WireJSON.memberBytes(Data(#"{"a":1,"a":2}"#.utf8), "a"))
        XCTAssertThrowsError(try WireJSON.parse(Data(#"{"a":"#.utf8)))
        let fields = try WireJSON.Object(WireJSON.parse(doc))
        XCTAssertEqual(try fields.string("type"), "x")
        XCTAssertEqual(fields.field("n")?.raw, "[1,2]")
        XCTAssertTrue(WireJSON.isValid(doc))
    }
}
