import Foundation
import XCTest

@testable import RealtimeConnection

/// The wire JSON reader on bytes the fixtures cannot carry as JSON text (Foundation's fixture loader drops a leading
/// U+FEFF, and a JSON string cannot hold invalid UTF-8), and its field rules.
final class SessionWireTests: XCTestCase {
    func testRefusesBytesFixturesCannotCarry() {
        let docs: [String: [UInt8]] = [
            "bom": [0xEF, 0xBB, 0xBF] + Array("{}".utf8),
            "invalid utf-8": Array("{\"a\":\"".utf8) + [0xFF] + Array("\"}".utf8),
            "utf-8 surrogate": Array("{\"a\":\"".utf8) + [0xED, 0xA0, 0x80] + Array("\"}".utf8),
            "overlong": Array("{\"a\":\"".utf8) + [0xC0, 0xAF] + Array("\"}".utf8),
            "above U+10FFFF": Array("{\"a\":\"".utf8) + [0xF4, 0x90, 0x80, 0x80] + Array("\"}".utf8),
            "nbsp outside": Array("{\"a\":".utf8) + [0xC2, 0xA0] + Array("1}".utf8),
            "utf-16": Array("{}".utf16).flatMap { [UInt8($0 & 0xFF), UInt8($0 >> 8)] },
            "empty": [],
        ]
        for (name, doc) in docs {
            XCTAssertThrowsError(try WireJSON.parse(Data(doc)), name)
        }
        XCTAssertNoThrow(try WireJSON.parse(Data("{\"a\":\"\u{10FFFF}\u{7F}\"}".utf8)))
    }

    func testFieldRules() throws {
        let o = try WireJSON.Object(
            WireJSON.parse(Data(#"{"s":"ab","i":-0,"f":1.5,"n":null,"o":{"k":"v","z":null},"code":5e2}"#.utf8)))
        XCTAssertEqual(try o.string("s"), "ab")
        XCTAssertNil(try o.string("n"))
        XCTAssertThrowsError(try o.string("i"))
        XCTAssertEqual(try o.int("i"), 0)
        XCTAssertThrowsError(try o.int("f"))
        XCTAssertEqual(try o.stringMap("o"), ["k": "v", "z": ""])
        XCTAssertEqual(try o.code("code"), "5e2")
    }

    func testDepthLimitWithoutRecursion() {
        func nested(_ n: Int) -> Data { Data((String(repeating: "[", count: n) + String(repeating: "]", count: n)).utf8) }
        XCTAssertNoThrow(try WireJSON.parse(nested(10_000)))
        XCTAssertThrowsError(try WireJSON.parse(nested(10_001)))
    }

    func testMintBodyRefusesNonUTF8Sessions() {
        var utf16 = Data([0xFF, 0xFE])
        for u in "{\"type\":\"realtime\"}".utf16 { utf16.append(contentsOf: [UInt8(u & 0xFF), UInt8(u >> 8)]) }
        XCTAssertThrowsError(try Minter.body(session: utf16, lifetimeSeconds: 60))
        XCTAssertThrowsError(try Minter.body(session: Data([0xEF, 0xBB, 0xBF]) + Data("{}".utf8), lifetimeSeconds: 60))
        XCTAssertNoThrow(try Minter.body(session: Data("{}".utf8), lifetimeSeconds: 60))
    }

    func testStrictBase64MatchesGo() {
        let cases: [(String, [UInt8]?)] = [
            ("", []), ("AA==", [0]), ("AAE=", [0, 1]), ("AAEC", [0, 1, 2]), ("AB==", [0]),
            ("AAE", nil), ("AA=", nil), ("AAEC====", nil), ("AAE=AAE=", nil), ("AA\r\nEC", nil), ("A===", nil),
            ("AA=A", nil), ("AA EC", nil),
        ]
        for (s, want) in cases {
            XCTAssertEqual(StrictBase64.decode(s).map { Array($0) }, want, s.debugDescription)
        }
    }
}
