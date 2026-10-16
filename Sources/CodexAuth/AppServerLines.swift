#if os(macOS)
import Darwin
import RealtimeConnection

/// The child's output split into lines inside one buffer of its own, as go/codexapp/lines.go splits it, so nothing the
/// child wrote is left in memory the package freed: a buffer outgrown is wiped as it is released, and so are the bytes
/// left behind when the unread ones move to the front. The caller wipes each line once it is handled, and ``wipe()``
/// the rest. A line longer than ``AppServerClient/maxLine`` before its newline is too long; a last line ended by the
/// end of the stream is still a line.
final class LineReader {
    enum Next {
        /// A line without its newline, a view of the buffer valid until the next call; `last` when the stream ended
        /// it.
        case line(UnsafeMutableRawBufferPointer, last: Bool)
        case tooLong
        case ended
    }

    /// Reads into the buffer given and returns how many bytes it read, more than zero; nil once the stream has ended.
    private let read: (UnsafeMutableRawBufferPointer) -> Int?
    private var buf = OwnedBytes(count: 64 << 10)
    /// The bytes not yet returned are `buf[start..<end]`; those before `scanned` hold no newline.
    private var start = 0, end = 0, scanned = 0
    private var ended = false

    init(read: @escaping (UnsafeMutableRawBufferPointer) -> Int?) { self.read = read }

    func next() -> Next {
        while true {
            let bytes = buf.bytes
            if let nl = bytes[scanned..<end].firstIndex(of: 0x0A) {
                let line = UnsafeMutableRawBufferPointer(rebasing: bytes[start..<nl])
                start = nl + 1
                scanned = start
                return .line(line, last: false)
            }
            scanned = end
            if end - start > AppServerClient.maxLine { return .tooLong }
            if ended {
                guard start < end else { return .ended }
                let line = UnsafeMutableRawBufferPointer(rebasing: bytes[start..<end])
                start = end
                return .line(line, last: true)
            }
            if end == bytes.count { makeRoom() }
            guard let n = read(UnsafeMutableRawBufferPointer(rebasing: buf.bytes[end...])) else {
                ended = true
                continue
            }
            end += n
        }
    }

    /// Moves the unread bytes to the front and wipes what they leave behind; when they fill the buffer, moves them into
    /// one twice the size, at most a line and its newline, and the one outgrown is wiped as it is released.
    private func makeRoom() {
        let unread = end - start
        if start > 0 {
            let bytes = buf.bytes
            bytes.baseAddress!.copyMemory(from: bytes.baseAddress! + start, byteCount: unread)
            _ = memset_s(bytes.baseAddress! + unread, end - unread, 0, end - unread)
        } else {
            let grown = OwnedBytes(count: Swift.min(2 * buf.bytes.count, AppServerClient.maxLine + 1))
            grown.bytes.copyMemory(from: UnsafeRawBufferPointer(rebasing: buf.bytes[0..<end]))
            buf = grown
        }
        scanned -= start
        (start, end) = (0, unread)
    }

    /// Wipes the buffer, the bytes not yet returned included.
    func wipe() {
        _ = memset_s(buf.bytes.baseAddress, buf.bytes.count, 0, buf.bytes.count)
        (start, end, scanned) = (0, 0, 0)
    }

    /// Reads the rest of the stream into the buffer, which the caller has wiped, wiping each read, so a child that
    /// keeps writing does not block on a full pipe; returns when the stream ends.
    func drain() {
        while !ended {
            guard let n = read(buf.bytes) else {
                ended = true
                break
            }
            _ = memset_s(buf.bytes.baseAddress, n, 0, n)
        }
    }
}

/// Wipes `bytes` in place.
func wipe(_ bytes: UnsafeMutableRawBufferPointer) {
    if let base = bytes.baseAddress { _ = memset_s(base, bytes.count, 0, bytes.count) }
}
#endif
