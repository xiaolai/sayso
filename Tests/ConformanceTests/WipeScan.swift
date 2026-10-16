#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection
import XCTest

/// Builds documents in a memory scan's pages and knows what the scan should find, as go/codexapp/scan_test.go does:
/// each needle, then its first 16 bytes, once for every copy the reader keeps and never for one it drops. A buffer
/// outgrown part way through a needle holds its first bytes without the rest, so a fragment is found too.
final class WipeScan {
    enum Part {
        case text(String)
        /// A fresh needle, as it is.
        case needle
        /// A fresh needle written as \u escapes, so only a decoded copy holds its bytes.
        case escaped
        /// A fresh needle the reader keeps one copy of, as it is or escaped.
        case kept, keptEscaped
        /// A fresh needle registered with the scrubber before the document is read. The `String` it is registered
        /// from is held until the scan is done, and counted: one copy.
        case registered
    }

    let scan: MemoryScan
    private(set) var needles: [Range<Int>] = []
    /// As ``found(freed:)`` counts.
    private(set) var want: [Int] = []
    private var registrations: [String] = []

    init(room: Int = 1 << 16) { scan = MemoryScan(room: room) }

    /// Appends the document `parts` make to the pages and returns it.
    func emit(_ parts: [Part]) -> UnsafeRawBufferPointer {
        let start = scan.mark
        for p in parts {
            if case .text(let s) = p {
                scan.append(s)
                continue
            }
            let n = scan.needle()
            scan.prefix(n, 16)
            needles.append(n)
            switch p {
            case .kept, .keptEscaped, .registered: want += [1, 1]
            default: want += [0, 0]
            }
            if case .registered = p {
                let s = String(decoding: scan.bytes(n), as: UTF8.self)
                Secret.register(s)
                registrations.append(s)
            }
            switch p {
            case .escaped, .keptEscaped:
                for b in scan.bytes(n) { scan.append(String(format: "\\u%04x", b)) }
            default:
                scan.append(copyOf: n)
            }
        }
        return scan.bytes(start..<scan.mark)
    }

    /// The bytes of the needle `k`, for a test to compare a kept value with in place.
    func needle(_ k: Int) -> UnsafeRawBufferPointer { scan.bytes(needles[k]) }

    /// Each needle, then its first 16 bytes: how often they were in a block freed while `body` ran, plus how often
    /// they are in memory now.
    func found(during body: () throws -> Void) rethrows -> [Int] {
        let freed = try scan.frees(during: body)
        return zip(freed, scan.occurrences()).map { $0 + $1 }
    }

    /// Fails unless `found` is what the reader should leave.
    func check(_ found: [Int], file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertEqual(
            found, want, "each needle, then each one's first 16 bytes: copies freed unwiped, or left, by the reader",
            file: file, line: line)
    }
}
#endif
