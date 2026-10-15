import SaidGuard
import XCTest

final class SaidGuardTests: XCTestCase {
    /// Swift declares the spell-out capabilities of Fixtures.swift, so a true expectation is exempt only when it
    /// requires a capability outside them; a false expectation binds every implementation (spec/parity.md).
    func testSaidGuard() throws {
        try runFixtures("said-guard") { c in
            guard let script = c.object["script"] as? String, let transcript = c.object["transcript"] as? String,
                let language = c.object["language"] as? String, let expect = c.object["expect"] as? Bool
            else { return "bad case" }
            let got = saidMatches(transcript, script, language: language)
            if got == expect { return nil }
            if expect && c.requires.contains(where: { !Results.capabilities.contains($0) }) { return "exempt" }
            return got ? "got match" : "got no match"
        }
    }
}
