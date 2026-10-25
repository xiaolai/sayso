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
        XCTAssertEqual(e.description, #"AppServerEvent{notification "item/completed", 27 bytes}"#)
        XCTAssertEqual(
            AppServerEvent(method: "approve", params: Data(), isServerRequest: true).description,
            #"AppServerEvent{server request "approve", 0 bytes}"#)
        XCTAssertEqual(e.paramsObject()?["text"] as? String, "canary-said-this")
    }

    /// A method the child controls cannot forge a log line or reach a terminal: control and format characters are
    /// escaped, as Go's strconv.Quote escapes them.
    func testMethodIsEscaped() {
        for m in ["a\nlevel=ERROR msg=forged", "a\rb", "a\u{1B}[31mred", "a\u{2028}b", "a\u{202E}b", "a\u{7F}b"] {
            let e = AppServerEvent(method: m, params: Data(), isServerRequest: false)
            var dumped = ""
            dump(e, to: &dumped)
            // dump writes one line per child; within each, nothing may be a control or format character.
            for s in [e.description, e.debugDescription, "\(e)"] + dumped.split(separator: "\n").map(String.init) {
                let bad = s.unicodeScalars.contains {
                    [.control, .format, .lineSeparator, .paragraphSeparator].contains($0.properties.generalCategory)
                }
                XCTAssertFalse(bad, "\(m.debugDescription) formats as \(s.debugDescription)")
            }
        }
        XCTAssertEqual(
            AppServerEvent(method: "a\nb\u{1B}\"\\", params: Data(), isServerRequest: false).description,
            #"AppServerEvent{notification "a\nb\x1b\"\\", 0 bytes}"#)
    }

    /// The auth method is the child's too, and is escaped the same way, as Go's %q writes it.
    func testAuthMethodIsEscaped() {
        for m in ["a\nlevel=ERROR msg=forged", "a\u{1B}[31mred", "a\u{202E}b"] {
            let st = AuthStatus(method: m, requiresOpenAIAuth: true, token: "")
            var dumped = ""
            dump(st, to: &dumped)
            for s in [st.description, st.debugDescription] + dumped.split(separator: "\n").map(String.init) {
                let bad = s.unicodeScalars.contains {
                    [.control, .format, .lineSeparator, .paragraphSeparator].contains($0.properties.generalCategory)
                }
                XCTAssertFalse(bad, "\(m.debugDescription) formats as \(s.debugDescription)")
            }
        }
        XCTAssertEqual(
            AuthStatus(method: "a\nb", requiresOpenAIAuth: true, token: "").description, #"AuthStatus{method: "a\nb"}"#)
    }
}
#endif
