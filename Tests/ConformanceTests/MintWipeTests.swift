#if os(macOS)
import Darwin
import Foundation
@testable import RealtimeConnection
import XCTest

/// No byte of a mint reply outlives its reading but the secret kept: a string is decoded once, into the `String`
/// kept, and the value is checked before it is made, so a refused reply leaves nothing (WipeScan.swift). URLSession's
/// own buffers are outside what the package owns, so the reply is handed to the reader as the client receives it.
final class MintWipeTests: XCTestCase {
    override func setUpWithError() throws {
        if MemoryScan.allocatorReplaced { throw XCTSkip("a sanitizer replaced the allocator the scan watches") }
    }

    /// A string value is decoded straight into its `String`: no array grown while decoding leaves a fragment, and
    /// the parsed document's copy is wiped.
    func testAStringIsDecodedOnceIntoItsString() throws {
        let w = WipeScan()
        let doc = w.emit([.text(#"{"v":""#), .keptEscaped, .text(String(repeating: "x", count: 2000) + #"","w":""#),
            .needle, .text(#""}"#)])
        var kept = ""
        let found = try w.found {
            kept = try XCTUnwrap(WireJSON.Object(WireJSON.parse(doc)).string("v"))
        }
        w.check(found)
        XCTAssertTrue(kept.utf8.starts(with: w.needle(0)), "the value decoded is not the one sent")
    }

    /// A member name is copied once, into an array of its size: no array grown while decoding it leaves a fragment.
    /// Names are not secrets, and the copy, the name's key, is freed as it is: found once freed and once in memory.
    func testAMemberNameIsCopiedOnceAtItsSize() throws {
        let w = WipeScan()
        let doc = w.emit([.text(#"{""#), .escaped, .text(String(repeating: "x", count: 2000) + #"":1}"#)])
        let found = try w.found { _ = try WireJSON.Object(WireJSON.parse(doc)) }
        XCTAssertEqual(found, [2, 2], "the name was copied more than once, or a fragment of it was left")
    }

    /// Reads the mint reply `parts` make, as the client hands it over, and checks what it leaves.
    private func mint(_ parts: [WipeScan.Part], room: Int = 1 << 16, file: StaticString = #filePath, line: UInt = #line)
        -> ClientSecret?
    {
        let w = WipeScan(room: room)
        let doc = w.emit(parts)
        var secret: ClientSecret?
        let found = w.found {
            var reply: HTTPReply? = HTTPReply(status: 200, headers: [:], body: Data(doc))
            secret = try? Minter.secret(from: &reply)
        }
        w.check(found, file: file, line: line)
        if let secret, !secret.bearer.utf8.elementsEqual(w.needle(0)) {
            XCTFail("the secret kept is not the value sent", file: file, line: line)
        }
        return secret
    }

    func testAMintKeepsOnlyItsSecret() {
        XCTAssertNotNil(mint([.text(#"{"value":""#), .kept, .text(#"","expires_at":1800000600,"session":{"k":""#),
            .needle, .text(#""},"x":[""#), .escaped, .text(#""]}"#)]))
        XCTAssertNotNil(mint([.text(#"{"expires_at":1800000600,"value":""#), .keptEscaped, .text(#""}"#)]))
    }

    func testARefusedMintLeavesNothing() {
        let long = String(repeating: "x", count: Minter.maximumSecretBytes)
        let cases: [[WipeScan.Part]] = [
            [.text(#"{"value":""#), .needle, .text(long + #"","expires_at":1800000600}"#)],
            [.text(#"{"value":""#), .escaped, .text(long + #"","expires_at":1800000600}"#)],
            [.text(#"{"value":[""#), .needle, .text(#""],"expires_at":1800000600}"#)],
            [.text(#"{"value":""#), .needle, .text(#"","expires_at":"soon"}"#)],
            [.text(#"{"value":""#), .escaped, .text(#"","expires_at":1.5}"#)],
            [.text(#"{"value":""#), .needle, .text(#"","expires_at":1800000600"#)],
            [.text(#"[""#), .needle, .text(#""]"#)],
        ]
        for (i, parts) in cases.enumerated() {
            XCTAssertNil(mint(parts), "case \(i) was not refused")
        }
    }
}
#endif
