import Foundation
import XCTest

/// The repeat form of spec/fixtures.md, as Fixtures.swift expands it. Each spec is parsed from JSON text, as the loader
/// reads it, so a bool or a fraction arrives as JSONSerialization delivers it.
final class FixtureRepeatTests: XCTestCase {
    private func object(_ json: String) throws -> [String: Any] {
        guard let o = try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any] else { throw FixtureError("not an object") }
        return o
    }

    func testExpands() throws {
        let good: [(String, String)] = [
            (#"{"repeat": "ab", "times": 3, "prefix": "<", "suffix": ">"}"#, "<ababab>"),
            (#"{"repeat": "一亿", "times": 2, "prefix": "", "suffix": ""}"#, "一亿一亿"),
            (#"{"suffix": "x", "prefix": "", "times": 1, "repeat": "dot "}"#, "dot x"),
            (#"{"repeat": "’", "times": 3, "prefix": "五", "suffix": "\n"}"#, "五’’’\n"),
            (#"{"repeat": "a", "times": 1048575, "prefix": "b", "suffix": ""}"#, "b" + String(repeating: "a", count: 1_048_575)),
        ]
        for (json, want) in good { XCTAssertEqual(try Fixtures.repeatText(object(json)), want, json) }
    }

    func testRefuses() throws {
        let bad = [
            #"{"times": 3, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 3, "suffix": ""}"#,
            #"{"repeat": "a", "times": 3, "prefix": ""}"#,
            #"{"repeat": null, "times": 3, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": null, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 3, "prefix": "", "suffix": "", "count": 3}"#,
            #"{"Repeat": "a", "times": 3, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "", "times": 3, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 0, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": -1, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 1.5, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 2.0, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 1e3, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": "3", "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": true, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 1048577, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 99999999999999999999, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "ab", "times": 524289, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 1048576, "prefix": "b", "suffix": ""}"#,
            #"{"repeat": 5, "times": 3, "prefix": "", "suffix": ""}"#,
            #"{"repeat": "a", "times": 3, "prefix": 1, "suffix": ""}"#,
            #"{"repeat": "a", "times": 3, "prefix": "", "suffix": ["x"]}"#,
        ]
        for json in bad {
            let spec = try object(json)  // parsed outside the assertion, so only the expansion's refusal counts
            XCTAssertThrowsError(try Fixtures.repeatText(spec), json)
        }
    }

    func testExpandsOnlyListedMembers() throws {
        let names = ["script", "transcript"]
        let note = try object(#"{"id": "x", "script": "a", "note": {"repeat": "a", "times": 2, "prefix": "", "suffix": ""}}"#)
        XCTAssertNotNil(try Fixtures.expandRepeats(note, names, id: "x")["note"] as? [String: Any])
        let c = try Fixtures.expandRepeats(
            object(#"{"id": "x", "script": {"repeat": "a ", "times": 3, "prefix": "<", "suffix": "b"}, "transcript": "c", "expect": false}"#),
            names, id: "x")
        XCTAssertEqual(c["script"] as? String, "<a a a b")
        XCTAssertEqual(c["transcript"] as? String, "c")
        XCTAssertEqual(c["expect"] as? Bool, false)
        // A malformed repeat fails the case and names it and its member.
        let bad = try object(#"{"script": "a", "transcript": {"repeat": "a", "times": -1, "prefix": "", "suffix": ""}}"#)
        XCTAssertThrowsError(try Fixtures.expandRepeats(bad, names, id: "x")) {
            XCTAssertTrue("\($0)".contains("fixture x: transcript"), "\($0)")
        }
    }
}
