import Foundation
@testable import SaidGuard
import XCTest

/// Regressions for the said guard's lexer, readings and spell-out found by the code-quality audit.
final class SaidGuardAuditTests: XCTestCase {
    /// A signed operand after spaces ("5 - - 2") keeps the minus before it spoken: "minus negative two" reads it, and
    /// "negative two" alone does not.
    func testASpacedSignedOperandKeepsTheMinus() {
        for script in ["5 - - 2", "5 - -2", "5 - − 2"] {
            XCTAssertTrue(saidMatches("five minus negative two", script, language: "en"), "\(script): refused")
            XCTAssertFalse(saidMatches("five negative two", script, language: "en"), "\(script): the minus may be dropped")
        }
    }

    /// A long run of spaces is lexed in time linear in its length: only a minus looks across the spaces around it.
    func testLongWhitespaceIsLinear() {
        func cpu() -> Double {
            var t = timespec()
            clock_gettime(CLOCK_THREAD_CPUTIME_ID, &t)
            return Double(t.tv_sec) + Double(t.tv_nsec) / 1e9
        }
        let text = "5" + String(repeating: " ", count: saidGuardMaxBytes - 2) + "x"
        let start = cpu()
        _ = saidMatches(text, text, language: "en")
        XCTAssertLessThan(cpu() - start, 5.0)
    }

    /// A domain whose first labels are single letters is a domain, not initials and a word.
    func testADomainMayStartWithSingleLetters() {
        XCTAssertTrue(saidMatches("x dot y dot com", "x.y.com", language: "en"))
        XCTAssertTrue(saidMatches("www dot x dot y dot com", "www.x.y.com", language: "en"))
        XCTAssertTrue(saidMatches("x y", "x.y", language: "en"), "initials alone stay initials")
    }

    /// A value beyond what the spell-out spells exactly is read as written only: French spells
    /// 100000000000000,000001 "cent billions", dropping the fraction.
    func testSpellOutRefusesValuesBeyondItsPrecision() {
        XCTAssertFalse(saidMatches("cent billions", "100000000000000,000001", language: "fr"))
        XCTAssertTrue(saidMatches("douze virgule cinq", "12,5", language: "fr"))
    }

    /// Korean spells -1.5 "마이너스 이점오" (minus 2.5); a negative fraction reads as minus and its positive spelling.
    func testSpellOutNegativeFractionKeepsItsMagnitude() {
        XCTAssertFalse(saidMatches("마이너스 이점오", "-1.5", language: "ko"))
        XCTAssertTrue(saidMatches("마이너스 일점오", "-1.5", language: "ko"))
        XCTAssertTrue(saidMatches("moins un virgule cinq", "-1,5", language: "fr"))
        XCTAssertTrue(saidMatches("마이너스 십이", "-12", language: "ko"))
    }

    /// Only ASCII digits are read digit by digit; anything else reads as nothing, never a wrong word or a trap.
    func testDigitsRejectNonDigits() {
        XCTAssertEqual(NumWords.digits("907"), ["nine", "zero", "seven"])
        for s in [":", "/", "a", "1:"] { XCTAssertEqual(NumWords.digits(s), [], s) }
    }

    /// A number's readings hold no reading twice.
    func testReadingsHoldNoDuplicates() {
        for (text, language) in [("42", "en"), ("1234", "en"), ("1,000", "en"), ("-7", "en"), ("42", "zh")] {
            let unit = svNumberUnits(text, language, nil)[0]
            XCTAssertEqual(unit.count, Set(unit).count, "\(text) in \(language): \(unit)")
        }
        for run in ["五", "二", "四十二", "一百一十"] {
            let unit = svHanUnit(run)
            XCTAssertEqual(unit.count, Set(unit).count, "\(run): \(unit)")
        }
    }
}
