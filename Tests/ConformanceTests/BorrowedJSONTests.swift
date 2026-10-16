#if os(macOS)
@testable import CodexAuth
import Foundation
@testable import RealtimeConnection
import XCTest

/// The in-place readers agree with the converting ones they replaced.
final class BorrowedJSONTests: XCTestCase {
    private func withDoc<T>(_ json: String, _ body: (WireJSON.Borrowed) throws -> T) rethrows -> T? {
        var bytes = Array(json.utf8)
        return try bytes.withUnsafeMutableBytes { raw in
            guard let doc = WireJSON.Borrowed(UnsafeRawBufferPointer(raw)) else { return nil }
            return try body(doc)
        }
    }

    /// Finite as a Double exactly when Foundation's correctly rounded conversion is: around the overflow threshold,
    /// 2^1024 - 2^970, and far from it.
    func testFiniteAsDoubleMatchesTheConversion() {
        let t = "179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330"
            + "286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069"
            + "855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792"
        let below = String(t.dropLast()) + "1"
        var cases = [
            "0", "-0", "1", "-1", "1.5", "1e308", "1e309", "-1e309", "1e-400", "0e999999999999", "0.0e5",
            "1.7976931348623157e308", "1.7976931348623158e308", "1.7976931348623159e308", "-1.7976931348623159e308",
            "1.797693134862315807937289714053e308", "1.797693134862315807937289714054e308", t, "-" + t, below,
            t + ".0", t + ".0000001", below + ".99999", "0." + t + "e309", "0." + below + "e309", "000" + t,
            String(repeating: "9", count: 309), String(repeating: "9", count: 310), "1" + String(repeating: "0", count: 308),
            "17976931348623158079e289", "17976931348623158080e289", "1e99999999999999999999", "1E+308", "1E-308",
            "10e307", "100e306", "0.1e310",
        ]
        for _ in 0..<200 {
            let digits = String((0..<Int.random(in: 1...30)).map { _ in "0123456789".randomElement()! })
            cases.append("1" + digits + "e" + String(Int.random(in: 270...320)))
        }
        for c in cases {
            let want = Double(c).map(\.isFinite) ?? true
            let got = Array(c.utf8).withUnsafeBytes { finiteAsDouble($0) }
            XCTAssertEqual(got, want, c.count > 60 ? String(c.prefix(30)) + "…" + String(c.suffix(20)) : c)
        }
        // Not a number: the strict check's to refuse.
        for c in ["-", "1.", ".5", "1e", "1e+", "--1", "1-2"] {
            XCTAssertTrue(Array(c.utf8).withUnsafeBytes { finiteAsDouble($0) }, c)
        }
    }

    /// An integer within Int64, read in place, as Go decodes one into an int64.
    func testInt64() {
        let cases: [(String, Int64?)] = [
            ("0", 0), ("-0", 0), ("7", 7), ("-7", -7), ("9223372036854775807", .max),
            ("-9223372036854775808", .min), ("9223372036854775808", nil), ("-9223372036854775809", nil),
            ("1.0", nil), ("1e3", nil), ("\"1\"", nil), ("true", nil), ("null", nil),
        ]
        for (text, want) in cases {
            XCTAssertEqual(withDoc(text) { $0.int64($0.root) } ?? nil, want, text)
        }
    }

    /// Strings decoded in place equal JSONSerialization's decoding, escapes of every length included, and member
    /// names match as decoded bytes.
    func testStringsAndNamesDecodeAsFoundationDoes() throws {
        let strings = [
            #""""#, #""plain""#, #""\"\\\/\b\f\n\r\t""#, #""Aé€😀""#, #""é€😀""#,
            #""a\u0000b""#, #""\u007f\u0080߿ࠀ￿""#,
        ]
        for s in strings {
            let want = try JSONSerialization.jsonObject(with: Data(s.utf8), options: [.fragmentsAllowed]) as? String
            XCTAssertEqual(withDoc(s) { $0.string($0.root) } ?? nil, want, s)
            XCTAssertEqual(withDoc(s) { $0.stringEquals($0.root, want ?? "") }, true, s)
            XCTAssertEqual(withDoc(s) { $0.stringEquals($0.root, (want ?? "") + "x") }, false, s)
        }
        XCTAssertEqual(withDoc(#"{"a":1,"exp":2}"#) { d in d.member(d.root, "exp").flatMap { d.int64($0) } } ?? nil, 2)
        XCTAssertEqual(withDoc(#"{"exp":1}"#) { d in d.member(d.root, "EXP") }, .some(nil))
        XCTAssertEqual(withDoc(#"[1]"#) { d in d.member(d.root, "0") }, .some(nil))
        XCTAssertNil(withDoc(#"{"é":1,"é":2}"#) { _ in true }, "names that decode the same are a duplicate")
        XCTAssertEqual(withDoc(#"{"é":1,"é":2}"#) { _ in true }, true, "canonical equivalents are two names")
    }
}
#endif
