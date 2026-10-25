import Foundation

/// Bytes in memory of their own, wiped before it is freed (spec/credentials.md, hygiene). A reply that may hold a
/// credential is read into one and read where it lies, so it leaves no copy once released: a `Data` or an array grows
/// by moving its bytes and frees the old storage as it is. Unchecked Sendable: the bytes are written only by the one
/// owner that fills them, before anything else can see them.
package final class OwnedBytes: @unchecked Sendable {
    /// `count` bytes, zero until written.
    package let bytes: UnsafeMutableRawBufferPointer
    private let storage: UnsafeMutableRawBufferPointer

    package init(count: Int) {
        storage = .allocate(byteCount: Swift.max(count, 1), alignment: 1)
        storage.initializeMemory(as: UInt8.self, repeating: 0)
        bytes = UnsafeMutableRawBufferPointer(rebasing: storage[0..<count])
    }

    /// A copy of `source`.
    package convenience init(copying source: UnsafeRawBufferPointer) {
        self.init(count: source.count)
        bytes.copyMemory(from: source)
    }

    deinit {
        _ = memset_s(storage.baseAddress, storage.count, 0, storage.count)
        storage.deallocate()
    }
}

/// A reply body read into memory of its own, at most `limit` bytes: sized from the length the sender declared and
/// grown by doubling when the body is longer, each buffer outgrown wiped as it is released. Noncopyable: copies would
/// share one buffer, and appending to both would write over each other's bytes.
struct BodyBuffer: ~Copyable {
    private var owned: OwnedBytes
    private(set) var count = 0
    let limit: Int

    /// `declared` is the length the sender announced, -1 when unknown.
    init(declared: Int64, limit: Int) {
        self.limit = limit
        owned = OwnedBytes(count: declared >= 0 ? Int(Swift.min(declared, Int64(limit))) : Swift.min(512, limit))
    }

    var isFull: Bool { count == limit }

    /// Appends `byte`; the caller stops at `limit` bytes.
    mutating func append(_ byte: UInt8) {
        precondition(count < limit, "a body past its limit")
        if count == owned.bytes.count {
            let grown = OwnedBytes(count: Swift.min(Swift.max(2 * count, 512), limit))
            grown.bytes.copyMemory(from: UnsafeRawBufferPointer(rebasing: owned.bytes[0..<count]))
            owned = grown  // the one outgrown is wiped as it is released
        }
        owned.bytes[count] = byte
        count += 1
    }

    /// The bytes read, as `Data` that holds this buffer: it is wiped once the last copy of the `Data` is released.
    /// Foundation copies a body of at most 14 bytes into the `Data` value itself and releases the buffer at once, so
    /// such a body is not wiped; none that short can carry a secret (a mint reply holds a value and an expiry, an
    /// error body an error object).
    var data: Data {
        guard count > 0, let base = owned.bytes.baseAddress else { return Data() }
        let owned = owned
        return Data(bytesNoCopy: base, count: count, deallocator: .custom { _, _ in withExtendedLifetime(owned) {} })
    }
}
