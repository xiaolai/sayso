import Foundation

/// Reads the JSON the provider sends with one strict rule set, which the Go module implements too (spec/session.md,
/// wire JSON): the whole document is one RFC 8259 value in valid UTF-8, with no duplicate member name in any object
/// and at most 10,000 levels of nesting; member names match exactly; null is absence. `JSONSerialization` and Go's
/// `encoding/json` disagree on each of these (case-folded names, merged or dropped duplicates, replaced invalid
/// UTF-8, rewritten numbers), which is why both implementations read the provider's JSON through this one grammar.
package enum WireJSON {
    /// Any document or field the rules refuse.
    struct Invalid: Error {}

    /// Go's `jsontext` limit: 10,000 levels are accepted, 10,001 are not.
    static let maximumDepth = 10_000

    /// Whether `data` is one strict JSON value. It is read in place and nothing of it is copied: auth.json and
    /// token replies hold credentials (AUTH-02).
    package static func isValid(_ data: Data) -> Bool {
        data.withUnsafeBytes { Borrowed($0) != nil }
    }

    /// Zeroes `bytes` in place and returns where they were, which differs from where they were before the call when
    /// another reference shared the storage: the write then copied it first and cleared only the copy.
    @discardableResult
    static func wipe(_ bytes: inout [UInt8]) -> UInt {
        bytes.withUnsafeMutableBytes { raw in
            if let base = raw.baseAddress { _ = memset_s(base, raw.count, 0, raw.count) }
            return UInt(bitPattern: raw.baseAddress)
        }
    }

    /// Validates `data` as one strict JSON value, whitespace around it allowed, and returns its root.
    /// The document's copy of the bytes is wiped once its last node is released, or at once when it is refused:
    /// the mint reply and app-server events hold secrets.
    static func parse<C: Collection>(_ data: C) throws -> Node where C.Element == UInt8 {
        let doc = Document(copying: data)
        // The validator reads the document's bytes in place, inside its lifetime, so the wipe finds them unshared.
        let range = try withExtendedLifetime(doc) { () throws -> Range<Int> in
            try doc.bytes.withUnsafeBytes { raw in
                var v = Validator(raw)
                return try v.document()
            }
        }
        return Node(doc: doc, range: range)
    }

    /// The exact bytes of the member `name` of the JSON object `data`, as the sender wrote them; nil when absent.
    /// Throws ``Invalid`` unless `data` is one strict JSON object. For the other modules of this package, which
    /// cannot see ``Node``: CodexAuth hands an app-server event's params on byte for byte, as Go does.
    package static func memberBytes<C: Collection>(_ data: C, _ name: String) throws -> Data? where C.Element == UInt8 {
        let root = try parse(data)
        guard let n = try root.members()[Array(name.utf8)] else { return nil }
        return n.data
    }

    /// A validated document's bytes, shared by its nodes and wiped when the last of them is released. Unchecked
    /// Sendable: `bytes` is written only by `deinit`, when no other reference to the document is left.
    final class Document: @unchecked Sendable {
        private(set) var bytes: [UInt8]
        private let address: UInt

        /// Copies `source` into storage of the document's own: `Array(source)` can share a whole-buffer slice's
        /// storage with the caller, whose copy the wipe would then leave behind.
        init<C: Collection>(copying source: C) where C.Element == UInt8 {
            var owned: [UInt8] = []
            owned.reserveCapacity(source.count)
            owned.append(contentsOf: source)
            bytes = owned
            address = owned.withUnsafeBytes { UInt(bitPattern: $0.baseAddress) }
        }

        deinit {
            // A reader that kept a copy of `bytes` beyond the document would make the wipe clear a fresh copy and
            // leave the original behind; every reader's copy ends before its node does.
            let wiped = WireJSON.wipe(&bytes)
            assert(wiped == address, "a copy of a parsed document outlived it, so its wipe cleared only a copy")
            assert(!bytes.contains { $0 != 0 }, "a parsed document was not wiped")
        }
    }

    /// One JSON value: its exact bytes, as received. Only a validated document's nodes exist, so reading one never
    /// meets malformed input.
    struct Node {
        let doc: Document
        let range: Range<Int>

        private var bytes: [UInt8] { doc.bytes }

        /// `{`, `[`, `"`, `0` (a number), `t`, `f` or `n`.
        var kind: UInt8 {
            let b = bytes[range.lowerBound]
            return (b == 0x2D || (b >= 0x30 && b <= 0x39)) ? 0x30 : b
        }

        var isNull: Bool { kind == UInt8(ascii: "n") }

        /// The value's exact bytes. Read through a node, every copy of the document's bytes ends while the node
        /// still holds the document.
        var data: Data { Data(bytes[range]) }

        /// The value's exact text.
        var raw: String { String(decoding: bytes[range], as: UTF8.self) }

        /// A string node's value, decoded straight into the `String`'s storage (``Scanner/string(_:at:atMost:)``).
        func string() throws -> String {
            guard kind == UInt8(ascii: "\""), let s = Scanner.string(bytes, at: range.lowerBound) else { throw Invalid() }
            return s
        }

        /// An object's members, keyed by the bytes of their names: a `String` key would merge canonically
        /// equivalent names ("\u{E9}" and "e\u{301}") that are different names to Go and to the validator.
        func members() throws -> [[UInt8]: Node] {
            guard kind == UInt8(ascii: "{") else { throw Invalid() }
            var out: [[UInt8]: Node] = [:]
            var i = Scanner.skipSpace(bytes, range.lowerBound + 1)
            while bytes[i] != UInt8(ascii: "}") {
                let name = Scanner.unescape(bytes, &i)
                i = Scanner.skipSpace(bytes, i) + 1  // the colon
                let start = Scanner.skipSpace(bytes, i)
                let end = Scanner.skipValue(bytes, start)
                out[name] = Node(doc: doc, range: start..<end)
                i = Scanner.skipSpace(bytes, end)
                if bytes[i] == UInt8(ascii: ",") { i = Scanner.skipSpace(bytes, i + 1) }
            }
            return out
        }

        /// An array's elements.
        func elements() throws -> [Node] {
            guard kind == UInt8(ascii: "[") else { throw Invalid() }
            var out: [Node] = []
            var i = Scanner.skipSpace(bytes, range.lowerBound + 1)
            while bytes[i] != UInt8(ascii: "]") {
                let end = Scanner.skipValue(bytes, i)
                out.append(Node(doc: doc, range: i..<end))
                i = Scanner.skipSpace(bytes, end)
                if bytes[i] == UInt8(ascii: ",") { i = Scanner.skipSpace(bytes, i + 1) }
            }
            return out
        }
    }

    /// An object's members, read with the field rules: absent and null are the same, and a present field of the
    /// wrong kind throws ``Invalid``.
    struct Object {
        let members: [[UInt8]: Node]

        init(_ node: Node) throws { members = try node.members() }

        func field(_ name: String) -> Node? {
            guard let n = members[Array(name.utf8)], !n.isNull else { return nil }
            return n
        }

        func string(_ name: String) throws -> String? { try field(name)?.string() }

        /// A number written as an integer, without fraction or exponent, within `Int64`.
        func int(_ name: String) throws -> Int64? {
            guard let n = field(name) else { return nil }
            guard n.kind == 0x30, let v = Int64(n.raw) else { throw Invalid() }
            return v
        }

        /// Any number as a `Double`; one out of range throws, as Go's `ParseFloat` refuses it.
        func double(_ name: String) throws -> Double? {
            guard let n = field(name) else { return nil }
            guard n.kind == 0x30, let v = Double(n.raw), v.isFinite else { throw Invalid() }
            return v
        }

        func object(_ name: String) throws -> Object? {
            guard let n = field(name) else { return nil }
            return try Object(n)
        }

        func array(_ name: String) throws -> [Node]? { try field(name)?.elements() }

        /// An object whose values are strings, null being "". Two names that are canonically equivalent are
        /// invalid: a `String`-keyed result cannot hold both, and Go refuses them too (spec/session.md, wire JSON). So
        /// is a name with a run of more than 30 combining marks, where Go's normaliser and Swift's equality disagree,
        /// and a name with a character not assigned in ``pinnedUnicode``, which this runtime's tables may not know.
        func stringMap(_ name: String) throws -> [String: String]? {
            guard let o = try object(name) else { return nil }
            var out: [String: String] = [:]
            for (k, n) in o.members {
                let key = String(decoding: k, as: UTF8.self)
                guard WireJSON.pinned(key), !longMarkRun(key) else { throw Invalid() }
                guard out[key] == nil else { throw Invalid() }
                out[key] = n.isNull ? "" : try n.string()
            }
            return out
        }

        /// A provider error code: a string as itself, any other value as its exact JSON text, "" for absence or null.
        func code(_ name: String) throws -> String {
            guard let n = field(name) else { return "" }
            return n.kind == UInt8(ascii: "\"") ? try n.string() : n.raw
        }
    }

    /// The Unicode version a metadata name's characters must be assigned in: that of the Swift runtime on the oldest
    /// supported systems (macOS 14, iOS 17), as in Go (`wire.PinnedUnicode`). A character's canonical combining class
    /// and decomposition never change once it is assigned, so for these characters this runtime's tables, whatever
    /// their version, count marks and compare names as Go's Unicode 17 tables do.
    static let pinnedUnicode = (major: 15, minor: 0)

    /// Whether every character of `s` is assigned in ``pinnedUnicode``, private-use ones included. A noncharacter has
    /// an age but no assignment, so it is refused, as Go's table refuses it.
    static func pinned(_ s: String) -> Bool {
        s.unicodeScalars.allSatisfy { u in
            guard let age = u.properties.age, u.properties.generalCategory != .unassigned else { return false }
            return (age.major, age.minor) <= pinnedUnicode
        }
    }

    /// The stream-safe bound: UAX #15 allows at most 30 consecutive non-starters.
    static let maximumMarkRun = 30

    /// A run of more than ``maximumMarkRun`` non-starters, counted in each scalar's canonical decomposition, as Go
    /// counts it.
    static func longMarkRun(_ s: String) -> Bool {
        var run = 0
        for scalar in s.unicodeScalars {
            for d in String(scalar).decomposedStringWithCanonicalMapping.unicodeScalars {
                if d.properties.canonicalCombiningClass == .notReordered {
                    run = 0
                } else {
                    run += 1
                    if run > maximumMarkRun { return true }
                }
            }
        }
        return false
    }

    /// A strict JSON document read where it lies (AUTH-02): validation, member lookup and comparison read the caller's
    /// bytes in place and copy none of them, so a value the caller does not keep never leaves the caller's buffer,
    /// which the caller wipes. ``string(_:)`` makes the one copy, of a value the caller keeps. The caller's bytes must
    /// outlive the value.
    package struct Borrowed {
        private let b: UnsafeRawBufferPointer
        /// The root value.
        package let root: Range<Int>

        /// nil unless `bytes` is one strict JSON value.
        package init?(_ bytes: UnsafeRawBufferPointer) {
            var v = Validator(bytes)
            guard let r = try? v.document() else { return nil }
            (b, root) = (bytes, r)
        }

        /// `{`, `[`, `"`, `0` (a number), `t`, `f` or `n`.
        package func kind(_ v: Range<Int>) -> UInt8 {
            let c = b[v.lowerBound]
            return (c == 0x2D || (c >= 0x30 && c <= 0x39)) ? 0x30 : c
        }

        package func isNull(_ v: Range<Int>) -> Bool { kind(v) == UInt8(ascii: "n") }

        /// The value of the member `name` of the object `object`; nil when it has none, or is no object. Names match
        /// as decoded bytes.
        package func member(_ object: Range<Int>, _ name: String) -> Range<Int>? {
            guard kind(object) == UInt8(ascii: "{") else { return nil }
            var i = Scanner.skipSpace(b, object.lowerBound + 1)
            while b[i] != UInt8(ascii: "}") {
                let at = i
                i = Scanner.skipSpace(b, Scanner.skipString(b, i)) + 1  // the colon
                let start = Scanner.skipSpace(b, i)
                let end = Scanner.skipValue(b, start)
                if Scanner.unescapedEquals(b, at, name.utf8) { return start..<end }
                i = Scanner.skipSpace(b, end)
                if b[i] == UInt8(ascii: ",") { i = Scanner.skipSpace(b, i + 1) }
            }
            return nil
        }

        /// Whether `v` is a string that decodes to exactly `s`.
        package func stringEquals(_ v: Range<Int>, _ s: String) -> Bool {
            kind(v) == UInt8(ascii: "\"") && Scanner.unescapedEquals(b, v.lowerBound, s.utf8)
        }

        /// The string `v` decoded, written straight into the new `String`'s storage; nil for any other value, and for
        /// one that decodes to more than `limit` bytes, which is refused before any `String` of it is made.
        package func string(_ v: Range<Int>, atMost limit: Int = .max) -> String? {
            guard kind(v) == UInt8(ascii: "\"") else { return nil }
            return Scanner.string(b, at: v.lowerBound, atMost: limit)
        }

        /// A number written as an integer, without fraction or exponent, within `Int64`, as Go decodes one into an
        /// int64; nil for any other value. Read digit by digit, in place.
        package func int64(_ v: Range<Int>) -> Int64? {
            guard kind(v) == 0x30 else { return nil }
            var i = v.lowerBound
            let negative = b[i] == UInt8(ascii: "-")
            if negative { i += 1 }
            var n: Int64 = 0  // accumulated negative, so Int64.min fits
            while i < v.upperBound {
                let c = b[i]
                guard c >= 0x30, c <= 0x39 else { return nil }  // a fraction or an exponent
                let (m, o1) = n.multipliedReportingOverflow(by: 10)
                let (d, o2) = m.subtractingReportingOverflow(Int64(c - 0x30))
                guard !o1, !o2 else { return nil }
                n = d
                i += 1
            }
            if negative { return n }
            return n == .min ? nil : -n
        }
    }

    /// Helpers that walk a document already validated.
    enum Scanner {
        static func isSpace(_ b: UInt8) -> Bool { b == 0x20 || b == 0x09 || b == 0x0A || b == 0x0D }

        static func skipSpace<B: RandomAccessCollection>(_ b: B, _ i: Int) -> Int
        where B.Element == UInt8, B.Index == Int {
            var i = i
            while i < b.count, isSpace(b[i]) { i += 1 }
            return i
        }

        /// The end of the value starting at `i`.
        static func skipValue<B: RandomAccessCollection>(_ b: B, _ i: Int) -> Int
        where B.Element == UInt8, B.Index == Int {
            switch b[i] {
            case UInt8(ascii: "\""):
                return skipString(b, i)
            case UInt8(ascii: "{"), UInt8(ascii: "["):
                var depth = 0
                var j = i
                while true {
                    switch b[j] {
                    case UInt8(ascii: "\""):
                        j = skipString(b, j)
                        continue
                    case UInt8(ascii: "{"), UInt8(ascii: "["): depth += 1
                    case UInt8(ascii: "}"), UInt8(ascii: "]"):
                        depth -= 1
                        if depth == 0 { return j + 1 }
                    default: break
                    }
                    j += 1
                }
            default:
                var j = i
                while j < b.count, !isSpace(b[j]), b[j] != UInt8(ascii: ","), b[j] != UInt8(ascii: "}"),
                    b[j] != UInt8(ascii: "]")
                {
                    j += 1
                }
                return j
            }
        }

        /// The end of the string starting at `i` (its opening quote), after the closing quote. It copies nothing: a
        /// skipped value may be a credential, and a copy could not be wiped.
        static func skipString<B: RandomAccessCollection>(_ b: B, _ i: Int) -> Int
        where B.Element == UInt8, B.Index == Int {
            var j = i + 1
            while b[j] != UInt8(ascii: "\"") {
                j += b[j] == UInt8(ascii: "\\") ? 2 : 1  // an escape's second byte is never a quote that ends it
            }
            return j + 1
        }

        /// The UTF-8 bytes of the string starting at `i` (its opening quote); `i` ends after the closing quote. They are
        /// counted first and written into an array of that size: one grown as it is written frees each outgrown
        /// buffer as it is.
        static func unescape(_ b: [UInt8], _ i: inout Int) -> [UInt8] {
            var count = 0
            var it = Unescaped(b, at: i)
            while it.next() != nil { count += 1 }
            var out: [UInt8] = []
            out.reserveCapacity(count)
            it = Unescaped(b, at: i)
            while let c = it.next() { out.append(c) }
            i = it.end
            return out
        }

        /// The string starting at `quote` decoded, written straight into the new `String`'s storage: its bytes are
        /// counted first, so no buffer grows and no other copy of the value is made. nil when it decodes to more than
        /// `limit` bytes, before any `String` is made.
        static func string<B: RandomAccessCollection>(_ b: B, at quote: Int, atMost limit: Int = .max) -> String?
        where B.Element == UInt8, B.Index == Int {
            var count = 0
            var it = Unescaped(b, at: quote)
            while it.next() != nil {
                count += 1
                if count > limit { return nil }
            }
            return String(unsafeUninitializedCapacity: count) { out in
                var it = Unescaped(b, at: quote)
                var k = 0
                while let c = it.next() {
                    out[k] = c
                    k += 1
                }
                return k
            }
        }

        /// Whether the string starting at `i` (its opening quote) decodes to exactly `other`. Nothing is copied.
        static func unescapedEquals<B: RandomAccessCollection, S: Sequence>(_ b: B, _ i: Int, _ other: S) -> Bool
        where B.Element == UInt8, B.Index == Int, S.Element == UInt8 {
            var it = Unescaped(b, at: i)
            var o = other.makeIterator()
            while true {
                let c = it.next()
                guard c == o.next() else { return false }
                if c == nil { return true }
            }
        }

        /// Whether the strings starting at `x` and `y` decode to the same bytes. Nothing is copied.
        static func unescapedEqual<B: RandomAccessCollection>(_ b: B, _ x: Int, _ y: Int) -> Bool
        where B.Element == UInt8, B.Index == Int {
            var p = Unescaped(b, at: x), q = Unescaped(b, at: y)
            while true {
                let c = p.next()
                guard c == q.next() else { return false }
                if c == nil { return true }
            }
        }

        /// The decoded bytes of a validated string, one at a time, read where the string lies: an escape's bytes are
        /// made in a register, so decoding a value, or comparing it, copies none of it into memory.
        struct Unescaped<B: RandomAccessCollection>: IteratorProtocol where B.Element == UInt8, B.Index == Int {
            private let b: B
            private var i: Int
            /// The rest of an escaped scalar's UTF-8 bytes, the next one highest.
            private var pending: UInt32 = 0
            private var pendingCount = 0

            /// The string whose opening quote is at `quote`.
            init(_ b: B, at quote: Int) {
                self.b = b
                i = quote + 1
            }

            /// Where the string ends, after its closing quote, once ``next()`` has returned nil.
            var end: Int { i + 1 }

            mutating func next() -> UInt8? {
                if pendingCount > 0 {
                    pendingCount -= 1
                    return UInt8(truncatingIfNeeded: pending >> (8 * pendingCount))
                }
                let c = b[i]
                if c == UInt8(ascii: "\"") { return nil }
                guard c == UInt8(ascii: "\\") else {
                    i += 1
                    return c
                }
                let e = b[i + 1]
                i += 2
                switch e {
                case UInt8(ascii: "b"): return 0x08
                case UInt8(ascii: "f"): return 0x0C
                case UInt8(ascii: "n"): return 0x0A
                case UInt8(ascii: "r"): return 0x0D
                case UInt8(ascii: "t"): return 0x09
                case UInt8(ascii: "u"):
                    var scalar = hex4(b, i)
                    i += 4
                    if scalar >= 0xD800 && scalar <= 0xDBFF {  // validated: a low surrogate escape follows
                        let low = hex4(b, i + 2)
                        i += 6
                        scalar = 0x10000 + ((scalar - 0xD800) << 10) + (low - 0xDC00)
                    }
                    switch scalar {
                    case ..<0x80:
                        return UInt8(scalar)
                    case ..<0x800:
                        (pending, pendingCount) = (0x80 | scalar & 0x3F, 1)
                        return UInt8(0xC0 | scalar >> 6)
                    case ..<0x10000:
                        (pending, pendingCount) = ((0x80 | scalar >> 6 & 0x3F) << 8 | (0x80 | scalar & 0x3F), 2)
                        return UInt8(0xE0 | scalar >> 12)
                    default:
                        pending = (0x80 | scalar >> 12 & 0x3F) << 16 | (0x80 | scalar >> 6 & 0x3F) << 8
                            | (0x80 | scalar & 0x3F)
                        pendingCount = 3
                        return UInt8(0xF0 | scalar >> 18)
                    }
                default: return e  // " \ /
                }
            }
        }

        static func hexDigit(_ c: UInt8) -> UInt32? {
            switch c {
            case 0x30...0x39: return UInt32(c - 0x30)
            case 0x41...0x46: return UInt32(c - 0x41 + 10)
            case 0x61...0x66: return UInt32(c - 0x61 + 10)
            default: return nil
            }
        }

        static func hex4<B: RandomAccessCollection>(_ b: B, _ i: Int) -> UInt32
        where B.Element == UInt8, B.Index == Int {
            (0..<4).reduce(0) { $0 << 4 | (hexDigit(b[i + $1]) ?? 0) }
        }
    }

    /// Checks a whole document without recursion, so nesting to the depth limit cannot exhaust a thread's stack.
    struct Validator {
        let b: UnsafeRawBufferPointer
        var i = 0

        init(_ b: UnsafeRawBufferPointer) { self.b = b }

        /// The names of one object seen so far: where each starts, by a seeded hash of its decoded bytes. Names are
        /// compared where they lie, so checking for a duplicate copies none of them. The hash mixes one byte at a
        /// time, as `Hasher`, which buffers what it is given, would not.
        final class Names {
            private var starts: [UInt64: [Int]] = [:]
            private let seed = UInt64.random(in: .min ... .max)

            /// Records the name at `at`; false when the object already has one that decodes the same.
            func insert(_ b: UnsafeRawBufferPointer, _ at: Int) -> Bool {
                var key = seed
                var it = Scanner.Unescaped(b, at: at)
                while let c = it.next() {
                    key = ((key ^ UInt64(c)) &* 0x9E37_79B9_7F4A_7C15).rotateLeft(29) &* 0xBF58_476D_1CE4_E5B9
                }
                if let seen = starts[key], seen.contains(where: { Scanner.unescapedEqual(b, $0, at) }) { return false }
                starts[key, default: []].append(at)
                return true
            }
        }

        private enum Frame {
            /// The names seen so far, in a reference so adding one does not copy the set (a copy per member made a
            /// wide object quadratic).
            case object(names: Names)
            case array
        }

        private func peek() -> UInt8? { i < b.count ? b[i] : nil }

        private mutating func skipSpace() { i = Scanner.skipSpace(b, i) }

        private mutating func expect(_ c: Character) throws {
            guard peek() == c.asciiValue else { throw Invalid() }
            i += 1
        }

        /// One value with optional surrounding whitespace, and nothing else.
        mutating func document() throws -> Range<Int> {
            skipSpace()
            let start = i
            var stack: [Frame] = []
            var needValue = true
            var end = start  // where the root value ends, recorded as it ends: no rescan of the document
            while true {
                skipSpace()
                if needValue {
                    guard let c = peek() else { throw Invalid() }
                    switch c {
                    case UInt8(ascii: "{"), UInt8(ascii: "["):
                        guard stack.count < WireJSON.maximumDepth else { throw Invalid() }
                        i += 1
                        skipSpace()
                        if c == UInt8(ascii: "{") {
                            if peek() == UInt8(ascii: "}") {
                                i += 1
                                if stack.isEmpty { end = i }
                                needValue = false
                            } else {
                                let names = Names()
                                _ = names.insert(b, try name())
                                stack.append(.object(names: names))
                                skipSpace()
                                try expect(":")
                            }
                        } else if peek() == UInt8(ascii: "]") {
                            i += 1
                            if stack.isEmpty { end = i }
                            needValue = false
                        } else {
                            stack.append(.array)
                        }
                        continue
                    case UInt8(ascii: "\""): _ = try string()
                    case UInt8(ascii: "t"): try literal("true")
                    case UInt8(ascii: "f"): try literal("false")
                    case UInt8(ascii: "n"): try literal("null")
                    default: try number()
                    }
                    if stack.isEmpty { end = i }
                    needValue = false
                    continue
                }
                // A value just ended.
                guard let top = stack.last else {
                    guard i == b.count else { throw Invalid() }  // a second value, or trailing garbage
                    return start..<end
                }
                guard let c = peek() else { throw Invalid() }
                switch (top, c) {
                case (.object(let names), UInt8(ascii: ",")):
                    i += 1
                    skipSpace()
                    guard names.insert(b, try name()) else { throw Invalid() }  // a duplicate member name
                    skipSpace()
                    try expect(":")
                    needValue = true
                case (.object, UInt8(ascii: "}")), (.array, UInt8(ascii: "]")):
                    i += 1
                    stack.removeLast()
                    if stack.isEmpty { end = i }
                case (.array, UInt8(ascii: ",")):
                    i += 1
                    needValue = true
                default:
                    throw Invalid()
                }
            }
        }

        /// A member name, validated; returns where it starts.
        private mutating func name() throws -> Int {
            guard peek() == UInt8(ascii: "\"") else { throw Invalid() }
            let at = i
            try string()
            return at
        }

        private mutating func literal(_ word: String) throws {
            for c in word.utf8 {
                guard peek() == c else { throw Invalid() }
                i += 1
            }
        }

        /// `-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`
        private mutating func number() throws {
            func digits() -> Int {
                let s = i
                while let c = peek(), c >= 0x30, c <= 0x39 { i += 1 }
                return i - s
            }
            if peek() == UInt8(ascii: "-") { i += 1 }
            guard let first = peek(), first >= 0x30, first <= 0x39 else { throw Invalid() }
            if first == 0x30 { i += 1 } else { _ = digits() }
            if peek() == UInt8(ascii: ".") {
                i += 1
                guard digits() > 0 else { throw Invalid() }
            }
            if peek() == UInt8(ascii: "e") || peek() == UInt8(ascii: "E") {
                i += 1
                if peek() == UInt8(ascii: "+") || peek() == UInt8(ascii: "-") { i += 1 }
                guard digits() > 0 else { throw Invalid() }
            }
        }

        /// A string in valid UTF-8 with no control character and only valid escapes; a surrogate escape must be a
        /// high one followed by a low one.
        private mutating func string() throws {
            try expect("\"")
            while true {
                guard let c = peek() else { throw Invalid() }
                switch c {
                case UInt8(ascii: "\""):
                    i += 1
                    return
                case UInt8(ascii: "\\"):
                    i += 1
                    guard let e = peek() else { throw Invalid() }
                    i += 1
                    switch e {
                    case UInt8(ascii: "\""), UInt8(ascii: "\\"), UInt8(ascii: "/"), UInt8(ascii: "b"),
                        UInt8(ascii: "f"), UInt8(ascii: "n"), UInt8(ascii: "r"), UInt8(ascii: "t"):
                        break
                    case UInt8(ascii: "u"):
                        let s = try hex4()
                        if s >= 0xDC00 && s <= 0xDFFF { throw Invalid() }  // a low surrogate alone
                        if s >= 0xD800 && s <= 0xDBFF {
                            guard peek() == UInt8(ascii: "\\"), i + 1 < b.count, b[i + 1] == UInt8(ascii: "u")
                            else { throw Invalid() }
                            i += 2
                            let low = try hex4()
                            guard low >= 0xDC00 && low <= 0xDFFF else { throw Invalid() }
                        }
                    default: throw Invalid()
                    }
                case 0x00..<0x20: throw Invalid()
                case 0x00..<0x80: i += 1
                default: try utf8Scalar()
                }
            }
        }

        private mutating func hex4() throws -> UInt32 {
            guard i + 4 <= b.count else { throw Invalid() }
            var v: UInt32 = 0
            for k in 0..<4 {
                guard let d = Scanner.hexDigit(b[i + k]) else { throw Invalid() }
                v = v << 4 | d
            }
            i += 4
            return v
        }

        /// One multi-byte UTF-8 scalar: no overlong form, no surrogate, nothing above U+10FFFF.
        private mutating func utf8Scalar() throws {
            let c = b[i]
            let (count, low, high): (Int, UInt8, UInt8)
            switch c {
            case 0xC2...0xDF: (count, low, high) = (2, 0x80, 0xBF)
            case 0xE0: (count, low, high) = (3, 0xA0, 0xBF)
            case 0xE1...0xEC, 0xEE...0xEF: (count, low, high) = (3, 0x80, 0xBF)
            case 0xED: (count, low, high) = (3, 0x80, 0x9F)
            case 0xF0: (count, low, high) = (4, 0x90, 0xBF)
            case 0xF1...0xF3: (count, low, high) = (4, 0x80, 0xBF)
            case 0xF4: (count, low, high) = (4, 0x80, 0x8F)
            default: throw Invalid()
            }
            guard i + count <= b.count else { throw Invalid() }
            guard b[i + 1] >= low, b[i + 1] <= high else { throw Invalid() }
            for k in 2..<max(2, count) where !(0x80...0xBF).contains(b[i + k]) { throw Invalid() }
            i += count
        }
    }
}

/// Standard base64 exactly as Go's `base64.StdEncoding` decodes it, except that CR and LF are refused rather than
/// skipped (spec/session.md): whole quanta of four, padding only at the end of the last one, trailing bits ignored.
/// `Data(base64Encoded:)` differs: it accepts surplus padding such as `AAEC====`.
enum StrictBase64 {
    static func decode(_ s: String) -> Data? {
        let b = Array(s.utf8)
        guard b.count % 4 == 0 else { return nil }
        func value(_ c: UInt8) -> UInt32? {
            switch c {
            case 0x41...0x5A: return UInt32(c - 0x41)
            case 0x61...0x7A: return UInt32(c - 0x61 + 26)
            case 0x30...0x39: return UInt32(c - 0x30 + 52)
            case UInt8(ascii: "+"): return 62
            case UInt8(ascii: "/"): return 63
            default: return nil
            }
        }
        var out = Data(capacity: b.count / 4 * 3)
        var q = 0
        while q < b.count {
            let last = q + 4 == b.count
            let pad = last ? (b[q + 3] == UInt8(ascii: "=") ? (b[q + 2] == UInt8(ascii: "=") ? 2 : 1) : 0) : 0
            var n: UInt32 = 0
            for k in 0..<(4 - pad) {
                guard let v = value(b[q + k]) else { return nil }
                n = n << 6 | v
            }
            n <<= UInt32(6 * pad)
            out.append(UInt8(n >> 16 & 0xFF))
            if pad < 2 { out.append(UInt8(n >> 8 & 0xFF)) }
            if pad < 1 { out.append(UInt8(n & 0xFF)) }
            q += 4
        }
        return out
    }
}

extension UInt64 {
    fileprivate func rotateLeft(_ n: UInt64) -> UInt64 { self << n | self >> (64 - n) }
}
