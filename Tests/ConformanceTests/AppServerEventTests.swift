#if os(macOS)
import CodexAuth
import Foundation
import XCTest

final class AppServerEventTests: XCTestCase {
    /// Formatting and reflection show an event's kind, method and size, never its params.
    func testFormattingHidesParams() {
        let e = AppServerEvent(
            method: "item/completed", params: Data(#"{"text":"canary-said-this"}"#.utf8), isServerRequest: false)
        var dumped = ""
        dump(e, to: &dumped)
        for s in [String(describing: e), String(reflecting: e), "\(e)", dumped, "\(Mirror(reflecting: e).children.map { $0.value })"] {
            XCTAssertFalse(s.contains("canary"), "params shown")
        }
        XCTAssertEqual(e.description, "AppServerEvent{notification item/completed, 27 bytes}")
        XCTAssertEqual(
            AppServerEvent(method: "approve", params: Data(), isServerRequest: true).description,
            "AppServerEvent{server request approve, 0 bytes}")
        XCTAssertEqual(e.paramsObject()?["text"] as? String, "canary-said-this")
    }
}
#endif
