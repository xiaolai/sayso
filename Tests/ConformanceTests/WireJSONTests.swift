import XCTest

@testable import RealtimeConnection

final class WireJSONTests: XCTestCase {
    /// A wide object validates in linear time: the names seen are not copied per member.
    func testWideObjectIsLinear() throws {
        var text = "{"
        for i in 0..<60_000 { text += (i == 0 ? "" : ",") + "\"k\(i)\":0" }
        text += "}"
        // This thread's CPU time, so a loaded machine's wait for a core is not counted.
        func cpu() -> Double {
            var t = timespec()
            clock_gettime(CLOCK_THREAD_CPUTIME_ID, &t)
            return Double(t.tv_sec) + Double(t.tv_nsec) / 1e9
        }
        let start = cpu()
        XCTAssertTrue(WireJSON.isValid(Data(text.utf8)))
        // Linear: well under a second. Copying the name set per member took about 27 s on this input.
        XCTAssertLessThan(cpu() - start, 10)
    }

    /// Canonically equivalent names are two members, each read and validated.
    func testEquivalentNamesAreTwoMembers() throws {
        let root = try WireJSON.parse(Data(#"{"é":1,"é":"ok"}"#.utf8))
        XCTAssertEqual(try root.members().count, 2)
    }
}
