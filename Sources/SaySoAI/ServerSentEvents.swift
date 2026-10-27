import Foundation

/// **A server-sent event stream framed from bytes**, by the WHATWG HTML parsing rules: CRLF, LF and a lone CR each end
/// a line, a leading byte order mark is skipped, a line starting with `:` is a comment, `data` lines join with `\n`, and
/// a blank line dispatches the event. Every other field is ignored, and an event with empty data is never dispatched.
///
/// **Not Foundation's line reader**: it drops blank lines, so no event would ever be dispatched, and it splits on
/// U+2028 and U+0085, which may sit unescaped inside a JSON string. Here only CR and LF end a line.
///
/// **Bounded as the bytes arrive**: a line, comments included, or one event's joined data past `cap` bytes is
/// `tooLarge` at the byte that passes it. The stream itself has no cap; the caller's timeouts bound it.
struct ServerSentEvents {
    private let cap: Int
    private var line: [UInt8] = []
    private var data: [UInt8] = []
    private var hasData = false
    /// The last byte ended a line with CR, so an LF right after it ends nothing.
    private var afterCR = false
    /// How many bytes of a leading byte order mark have been held back; `nil` once the stream is past its start.
    private var mark: Int? = 0

    private static let byteOrderMark: [UInt8] = [0xEF, 0xBB, 0xBF]

    init(cap: Int) {
        self.cap = cap
    }

    /// Takes one byte; the event's data when this byte dispatched one.
    mutating func take(_ byte: UInt8) throws(WireFailure) -> Data? {
        if let held = mark {
            if byte == Self.byteOrderMark[held] {
                mark = held + 1 == Self.byteOrderMark.count ? nil : held + 1
                return nil
            }
            mark = nil
            for byte in Self.byteOrderMark.prefix(held) { try append(byte) }
        }
        let wasAfterCR = afterCR
        afterCR = byte == 0x0D
        switch byte {
        case 0x0A where wasAfterCR: return nil
        case 0x0A, 0x0D: return try endLine()
        default:
            try append(byte)
            return nil
        }
    }

    /// The end of the stream: a last line without its line end is read, and an event not yet dispatched is.
    mutating func finish() throws(WireFailure) -> Data? {
        if !line.isEmpty { _ = try endLine() }
        return dispatch()
    }

    private mutating func append(_ byte: UInt8) throws(WireFailure) {
        guard line.count < cap else { throw .tooLarge }
        line.append(byte)
    }

    private mutating func endLine() throws(WireFailure) -> Data? {
        defer { line.removeAll(keepingCapacity: true) }
        if line.isEmpty { return dispatch() }
        if line.first == UInt8(ascii: ":") { return nil }
        let colon = line.firstIndex(of: UInt8(ascii: ":"))
        guard line[..<(colon ?? line.endIndex)].elementsEqual("data".utf8) else { return nil }
        var value = colon.map { line[($0 + 1)...] } ?? []
        if value.first == UInt8(ascii: " ") { value = value.dropFirst() }
        let added = (hasData ? 1 : 0) + value.count
        guard data.count + added <= cap else { throw .tooLarge }
        if hasData { data.append(0x0A) }
        data.append(contentsOf: value)
        hasData = true
        return nil
    }

    private mutating func dispatch() -> Data? {
        defer {
            data.removeAll(keepingCapacity: true)
            hasData = false
        }
        return data.isEmpty ? nil : Data(data)
    }
}
