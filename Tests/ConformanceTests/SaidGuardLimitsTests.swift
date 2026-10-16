import Foundation
import SaidGuard
import XCTest

final class SaidGuardLimitsTests: XCTestCase {
    /// Inputs that once crashed the generator or took seconds under the size cap (Go's limits_test.go). Each must
    /// return false, without crashing, within the bound. The bound is loose on purpose: an earlier memo-table matcher
    /// took seconds here, so it separates the algorithms without timing the machine; a debug build gets more room. The
    /// time is this thread's CPU time, so a loaded machine does not fail it (wall time measured 5–12 s at load 140
    /// where the CPU time was 0.9 s).
    func testBounds() {
        func cpu() -> Double {
            var t = timespec()
            clock_gettime(CLOCK_THREAD_CPUTIME_ID, &t)
            return Double(t.tv_sec) + Double(t.tv_nsec) / 1e9
        }
        #if DEBUG
            let bound = 30.0
        #else
            let bound = 5.0
        #endif
        let cases: [(name: String, script: String, transcript: String)] = [
            ("malformed-group", "0.1,000", "wrong"),
            ("malformed-sign", "-,", "wrong"),
            ("malformed-percent", "%1,0000%", "wrong"),
            ("english-rescan", String(repeating: "one ", count: 16383), "wrong"),
            ("english-phrase-rescan", String(repeating: "one hundred and ", count: 4095), "wrong"),
            ("domain-quadratic", String(repeating: "dot.dot ", count: 8191), String(repeating: "dot ", count: 16382) + "x"),
            ("domain-middle", String(repeating: "dot.dot ", count: 5000), String(repeating: "dot ", count: 12500) + "x"),
            ("han-repeat", String(repeating: "一亿", count: 10000), "wrong"),
            ("hyphen-chain", String(repeating: "ab", count: 8000), String(repeating: "a-b-", count: 8000) + "x"),
            ("dash-chain", String(repeating: "1-", count: 32767) + "1", "wrong"),
            (
                "domain-hyphen-labels", String(repeating: "e-mail.", count: 4000) + "com",
                String(repeating: "email dot e mail dot ", count: 2000) + "x"
            ),
            ("quotation-marks", String(repeating: "‘a’ b ", count: 6000), String(repeating: "a’ b ", count: 5999) + "x"),
            // A sentence's final punctuation looks past the run that follows it for a closing mark: once, not per mark.
            ("sentence-punctuation", String(repeating: "!", count: 65535) + "a", "wrong"),
            ("sentence-punctuation-mark", String(repeating: ".", count: 65533) + "’", "wrong"),
            ("free-dashes", String(repeating: "a - ", count: 16384), String(repeating: "a ", count: 16383) + "x"),
            ("number-word-codes", String(repeating: "TEN ", count: 16384), "wrong"),
            ("spellout-words", String(repeating: "vingt et un ", count: 5000), "wrong"),
            ("spellout-groups", String(repeating: "1 000 ", count: 10000), "wrong"),
            ("spellout-long-group", "1" + String(repeating: " 000", count: 16000), "wrong"),
            ("spellout-compounds", String(repeating: "zweitausendvierundzwanzig ", count: 2500), "wrong"),
        ]
        for c in cases {
            XCTAssertLessThanOrEqual(c.script.utf8.count, saidGuardMaxBytes, "\(c.name) would not exercise the matcher")
            XCTAssertLessThanOrEqual(c.transcript.utf8.count, saidGuardMaxBytes, "\(c.name) would not exercise the matcher")
            let start = cpu()
            var got = saidMatches(c.transcript, c.script, language: c.name == "spellout-compounds" ? "de" : c.name.hasPrefix("spellout") ? "fr" : "en")
            if c.name == "han-repeat" { got = got || saidMatches(c.transcript, c.script, language: "zh") }
            let elapsed = cpu() - start
            XCTAssertFalse(got, "\(c.name): matched, want false")
            XCTAssertLessThan(elapsed, bound, "\(c.name): took \(elapsed) s")
            print("\(c.name): \(String(format: "%.3f", elapsed)) s")
        }
    }

    /// The strict Han parser round-trips: repeated or out-of-order units and overflow are refused, never wrapped.
    func testHanStrict() {
        for s in ["一亿一亿", "一亿一亿一亿", "一万亿一万亿", "万万", "亿万", "零一百", "一百零二十", "一十零五", "一万零五千", "一五一十", "两十", "十十", "一千万万"] {
            XCTAssertNil(parseHanNumeral(s), s)
        }
        let good: [String: UInt64] = [
            "十": 10, "一十": 10, "十二": 12, "一百一": 110, "一百一十": 110, "一百零一": 101, "一百〇五": 105, "两百": 200,
            "两万五": 25000, "两万五千": 25000, "一万零五": 10005, "一万零五百": 10500, "一千零二十": 1020,
            "一亿五千万": 150_000_000, "一万亿": 1_000_000_000_000, "三千五": 3500,
        ]
        for (s, want) in good { XCTAssertEqual(parseHanNumeral(s), want, s) }
    }

    /// parseHanNumeral also reads digit strings, except the two-digit approximations and strings with 两.
    func testHanDigitStrings() {
        let good: [String: UInt64] = ["一〇八": 108, "二〇二四": 2024, "零": 0, "〇": 0, "两": 2, "七": 7, "一百一": 110]
        for (s, want) in good { XCTAssertEqual(parseHanNumeral(s), want, s) }
        for s in ["三五", "两三", "一两三", String(repeating: "九", count: 21), ""] { XCTAssertNil(parseHanNumeral(s), s) }
    }

    /// Over the cap never matches, even a script against itself.
    func testCap() {
        let s = String(repeating: "a ", count: saidGuardMaxBytes / 2 + 1)
        XCTAssertFalse(saidMatches(s, s, language: "en"))
        XCTAssertTrue(saidMatches("a a", "a a", language: "en"))
    }
}
