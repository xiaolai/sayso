import XCTest

final class ConformanceTests: XCTestCase {
    func testCredentials() throws { try runFixtures("credentials") { _ in "not implemented" } }
    func testMint() throws { try runFixtures("mint") { _ in "not implemented" } }
    func testSession() throws { try runFixtures("session") { _ in "not implemented" } }
    func testPCM16() throws { try runFixtures("pcm16") { _ in "not implemented" } }
    func testSaidGuard() throws { try runFixtures("said-guard") { _ in "not implemented" } }
}
