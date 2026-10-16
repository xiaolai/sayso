import Foundation
import XCTest

@testable import RealtimeConnection

/// The parser's copy of a document is wiped where it lives, not in a copy.
final class ParserWipeTests: XCTestCase {
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
