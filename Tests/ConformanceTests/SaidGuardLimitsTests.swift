import Foundation
import SaidGuard
import XCTest

final class SaidGuardLimitsTests: XCTestCase {
    /// The bound- fixtures are inputs that once crashed the generator or took seconds under the size cap; the
    /// conformance suite checks that each returns false (spec/fixtures.md). Here each must also return within the
    /// bound. The bound is loose on purpose: an earlier memo-table matcher took seconds on such inputs, so it separates
    /// the algorithms without timing the machine; a debug build gets more room. The time is this thread's CPU time, so a
    /// loaded machine does not fail it (wall time measured 5–12 s at load 140 where the CPU time was 0.9 s).
    func testBounds() throws {
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
        let cases = try Fixtures.load("said-guard").filter { $0.id.hasPrefix("bound-") }
        XCTAssertFalse(cases.isEmpty, "no bound- fixtures to time")
        for c in cases {
            guard let script = c.object["script"] as? String, let transcript = c.object["transcript"] as? String,
                let language = c.object["language"] as? String
            else { throw FixtureError("\(c.id): bad case") }
            XCTAssertLessThanOrEqual(script.utf8.count, saidGuardMaxBytes, "\(c.id) would not exercise the matcher")
            XCTAssertLessThanOrEqual(transcript.utf8.count, saidGuardMaxBytes, "\(c.id) would not exercise the matcher")
            let start = cpu()
            _ = saidMatches(transcript, script, language: language)
            let elapsed = cpu() - start
            XCTAssertLessThan(elapsed, bound, "\(c.id): took \(elapsed) s")
            print("\(c.id): \(String(format: "%.3f", elapsed)) s")
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
