import SaidGuard
import XCTest

/// The ICU spell-out readings beyond the fixtures: each reading is the locale's spelling of a value read strictly from
/// the script, so a reading that drops a sign, a fraction or a digit is never generated.
final class SaidGuardSpellOutTests: XCTestCase {
    private func check(_ script: String, _ transcript: String, _ language: String, _ want: Bool,
                       file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertEqual(saidMatches(transcript, script, language: language), want,
                       "\(language): \(script) / \(transcript)", file: file, line: line)
    }

    func testDigitsAreSpelled() {
        check("42", "zweiundvierzig", "de", true)
        check("-4", "minus vier", "de", true)
        check("-4", "vier", "de", false)
        check("1,5", "eins Komma fünf", "de", true)
        check("1,5", "eins fünf", "de", false)
        check("12 345", "douze mille trois cent quarante-cinq", "fr", true)
        check("21", "veintiuno", "es", true)
        check("21", "ventuno", "it", true)
        check("50%", "cinquante %", "fr", true)
        check("50%", "cinquante", "fr", false)
    }

    func testOnlyTheLocalesOwnFormIsANumber() {
        check("1,000", "mil", "pt", false)  // a decimal comma: not a thousand in Portuguese
        check("1.000", "mil", "pt", true)
        check("1,000", "एक हज़ार", "hi", true)
        check("007", "sept", "fr", false)  // a leading zero is not the locale's form of 7
        check("3/4", "trois quarts", "fr", false)  // separators other than the locale's are as written
    }

    func testNumberWordsReadAsDigits() {
        check("vier", "4", "de", true)
        check("vingt et un", "21", "fr", true)
        check("vingt-et-un", "21", "fr", true)
        check("deux mille trois cent quarante-cinq", "2345", "fr", true)
        check("deux mille trois cent quarante-cinq", "2 345", "fr", true)
        check("zweitausendvierundzwanzig", "2024", "de", true)  // German writes it as one word
        check("troisième", "3", "fr", false)  // ICU's lenient parser reads it as 3; it is not the spelling of 3
        check("douze mille un", "12001", "fr", false)  // beyond the table: as written only, a false mismatch
        check("eine", "1", "de", false)  // not the locale's spelling of a number
        check("vingt et un", "20 1", "fr", false)
    }

    func testOtherLanguagesHaveNoSpellOut() {
        check("4", "vier", "nl", false)
        check("4", "four", "en", true)
    }
}
