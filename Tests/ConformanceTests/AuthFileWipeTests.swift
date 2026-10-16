#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import XCTest

/// No byte of auth.json outlives a read except the access token kept: every buffer the file source reads it into,
/// validates it in, or decodes a member of, is a view of the one wiped buffer or is wiped before it is freed. A scan
/// of the process's writable memory, freed blocks included, finds no copy of a value the reader did not keep, on the
/// success path and on every refusal.
final class AuthFileWipeTests: XCTestCase {
    private enum Part {
        case text(String)
        /// A fresh needle, as it is.
        case needle
        /// A fresh needle written as \u escapes, so only a decoded copy holds its bytes.
        case escaped
        /// A fresh needle of digits, a number's text.
        case digits
        /// A JWT whose payload holds these parts, base64url-encoded, so only a decoded copy holds its bytes.
        case jwt([Part])
    }

    /// Writes the document `parts` make to a fresh auth.json, reads it with the file source, and returns the outcome
    /// with how often each needle was found: in a block freed during the read, or anywhere in memory after it.
    private func readCase(_ parts: [Part], file: StaticString = #filePath, line: UInt = #line) throws
        -> (state: CodexAuthState, found: [Int])
    {
        let scan = MemoryScan()
        func emit(_ parts: [Part]) {
            for p in parts {
                switch p {
                case .text(let s): scan.append(s)
                case .needle: scan.append(copyOf: scan.needle())
                case .escaped:
                    let n = scan.needle()
                    for b in scan.bytes(n) { scan.append(String(format: "\\u%04x", b)) }
                case .digits:
                    let n = scan.needle()
                    scan.digits(n)
                    scan.append(copyOf: n)
                case .jwt(let payload):
                    scan.append("eyJhbGciOiJub25lIn0.")
                    let start = scan.mark
                    emit(payload)
                    let end = scan.mark
                    let encoded = base64url(scan.bytes(start..<end))
                    // The payload's plain bytes are replaced by their encoding; the needles have pages of their own.
                    scan.reset(to: start)
                    scan.append(count: encoded.count) { out in
                        for k in 0..<encoded.count { out[k] = encoded[k] }
                    }
                    scan.append(".c2ln")
                }
            }
        }
        let docStart = scan.mark
        emit(parts)
        let doc = scan.bytes(docStart..<scan.mark)

        let home = try XCTUnwrap(realpath(NSTemporaryDirectory(), nil).map { p in defer { free(p) }; return String(cString: p) })
            + "/sayso-wipe-\(UUID().uuidString)"
        let codexHome = home + "/codex"
        try FileManager.default.createDirectory(atPath: codexHome, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: home) }
        let fd = open(codexHome + "/auth.json", O_CREAT | O_WRONLY | O_TRUNC | O_CLOEXEC, 0o600)
        XCTAssertGreaterThanOrEqual(fd, 0, file: file, line: line)
        XCTAssertEqual(write(fd, doc.baseAddress, doc.count), doc.count, file: file, line: line)
        close(fd)

        var state = CodexAuthState.ready
        let loc = try AuthFile.location(codexHome: codexHome, home: home, confine: home)
        defer { loc.close() }
        let freed = scan.frees {
            do {
                _ = try AuthFile.read(loc, onRetry: nil)
            } catch let e as CodexAuthError {
                state = e.state
            } catch {
                state = .codexError
            }
        }
        let live = scan.occurrences()
        return (state, zip(freed, live).map { $0 + $1 })
    }

    /// Base64url without padding; the result is an array of the encoding's bytes, which hold no needle.
    private func base64url(_ b: UnsafeRawBufferPointer) -> [UInt8] {
        let alphabet = Array("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_".utf8)
        var out: [UInt8] = []
        var i = 0
        while i < b.count {
            let n = min(3, b.count - i)
            var v: UInt32 = 0
            for k in 0..<3 { v = v << 8 | (k < n ? UInt32(b[i + k]) : 0) }
            for k in 0..<(n + 1) { out.append(alphabet[Int((v >> (18 - 6 * k)) & 63)]) }
            i += 3
        }
        return out
    }

    private func assertNoCopy(
        _ parts: [Part], _ state: CodexAuthState, file: StaticString = #filePath, line: UInt = #line
    ) throws {
        let (got, found) = try readCase(parts, file: file, line: line)
        XCTAssertEqual(got, state, file: file, line: line)
        XCTAssertEqual(
            found, [Int](repeating: 0, count: found.count), "a copy was freed unwiped, or outlived the read", file: file,
            line: line)
    }

    /// The scan finds bytes copied to the heap and freed unwiped: what the other tests rely on.
    func testTheScanFindsAFreedCopy() {
        let scan = MemoryScan()
        let n = scan.needle()
        let copy = malloc(n.count + 64)!
        memcpy(copy + 32, scan.bytes(n).baseAddress, n.count)
        XCTAssertGreaterThan(scan.occurrences()[0], 0, "the scan missed a live copy")
        let freed = scan.frees { free(copy) }
        XCTAssertEqual(freed, [1], "the logger missed a small copy freed unwiped")
        let big = malloc(200_000)!
        memcpy(big + 100_000, scan.bytes(n).baseAddress, n.count)
        XCTAssertEqual(scan.frees { free(big) }, [1], "the logger missed a large copy freed unwiped")
        let wiped = malloc(n.count)!
        memcpy(wiped, scan.bytes(n).baseAddress, n.count)
        XCTAssertEqual(scan.frees { _ = memset_s(wiped, n.count, 0, n.count); free(wiped) }, [0])
    }

    func testASignInKeepsOnlyTheAccessToken() throws {
        try assertNoCopy(
            [
                .text(#"{"auth_mode":"chatgpt","tokens":{"access_token":""#),
                .jwt([.text(#"{"exp":1800003600,"c":""#), .needle, .text(#"","e":""#), .escaped, .text(#""}"#)]),
                .text(#"","refresh_token":""#), .needle, .text(#"","id_token":{"n":[""#), .escaped,
                .text(#""]}},"OPENAI_API_KEY":null,""#), .needle, .text(#"":1,""#), .escaped,
                .text(#"":2,"last_refresh":"#), .digits, .text("}"),
            ], .ready)
    }

    func testARefusedSignInLeavesNothing() throws {
        try assertNoCopy([.text(#"{"OPENAI_API_KEY":""#), .needle, .text(#""}"#)], .apiKey)
        try assertNoCopy([.text(#"{"personal_access_token":""#), .escaped, .text(#""}"#)], .unsupportedAuth)
        try assertNoCopy([.text(#"{"auth_mode":""#), .needle, .text(#""}"#)], .codexError)
        try assertNoCopy([.text(#"{"auth_mode":""#), .escaped, .text(#""}"#)], .codexError)
        try assertNoCopy([.text(#"{"auth_mode":{"k":""#), .needle, .text(#""}}"#)], .codexError)
        try assertNoCopy([.text(#"{"auth_mode":[""#), .escaped, .text(#""]}"#)], .codexError)
        try assertNoCopy([.text(#"{"tokens":[""#), .needle, .text(#""]}"#)], .codexError)
        try assertNoCopy([.text(#"{"tokens":""#), .escaped, .text(#""}"#)], .codexError)
        try assertNoCopy([.text(#"{"tokens":{"access_token":{"k":""#), .needle, .text(#""}}}"#)], .codexError)
        try assertNoCopy([.text(#"{"tokens":{"access_token":7,"refresh_token":""#), .needle, .text(#""}}"#)], .codexError)
    }

    func testAnUnreadableFileLeavesNothing() throws {
        // A duplicate name, cut short, invalid UTF-8, a number out of range: refused by validation, each holding a
        // needle the validator read.
        try assertNoCopy([.text(#"{""#), .escaped, .text(#"":1,"x":""#), .needle, .text(#"","x":2}"#)], .codexError)
        try assertNoCopy([.text(#"{"tokens":{"refresh_token":""#), .needle], .codexError)
        try assertNoCopy([.text(#"{"k":""#), .needle, .text("\u{1}\"}")], .codexError)
        try assertNoCopy([.text(#"{"n":"#), .digits, .text("e999,\"k\":\""), .needle, .text(#""}"#)], .codexError)
        try assertNoCopy([.text(#"[""#), .needle, .text(#""]"#)], .codexError)
    }
}
#endif
