import XCTest

@testable import RealtimeConnection

final class WireJSONTests: XCTestCase {
    /// A wide object validates in linear time: the names seen are not copied per member.
    func testWideObjectIsLinear() throws {
        var text = "{"
        for i in 0..<60_000 { text += (i == 0 ? "" : ",") + "\"k\(i)\":0" }
        text += "}"
        let start = Date()
        XCTAssertTrue(WireJSON.isValid(Data(text.utf8)))
        // Linear: well under a second. Copying the name set per member took about 27 s on this input.
        XCTAssertLessThan(Date().timeIntervalSince(start), 10)
    }

    /// Canonically equivalent names are two members, each read and validated.
    func testEquivalentNamesAreTwoMembers() throws {
        let root = try WireJSON.parse(Data(#"{"é":1,"é":"ok"}"#.utf8))
        XCTAssertEqual(try root.members().count, 2)
    }
}
