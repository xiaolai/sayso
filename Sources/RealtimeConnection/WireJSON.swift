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

    /// Whether `data` is one strict JSON value. CodexAuth reads Codex's replies and files through it.
    /// The bytes it validates are wiped afterwards: auth.json and token replies hold credentials (AUTH-02).
    package static func isValid(_ data: Data) -> Bool {
        var bytes = Array(data)
        // The validator shares the buffer; it is released before the wipe, or the wipe would copy the buffer first
        // and clear only the copy.
        let valid: Bool = {
            var v = Validator(bytes)
            return (try? v.document()) != nil
        }()
        bytes.withUnsafeMutableBytes { raw in
            if let base = raw.baseAddress { _ = memset_s(base, raw.count, 0, raw.count) }
        }
        return valid
    }

    /// Validates `data` as one strict JSON value, whitespace around it allowed, and returns its root.
    static func parse(_ data: Data) throws -> Node {
        let doc = Document(Array(data))
        var v = Validator(doc.bytes)
        let range = try v.document()
        return Node(doc: doc, range: range)
    }

    final class Document: Sendable {
        let bytes: [UInt8]
        init(_ bytes: [UInt8]) { self.bytes = bytes }
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

        /// The value's exact text.
        var raw: String { String(decoding: bytes[range], as: UTF8.self) }

        /// A string node's value.
        func string() throws -> String {
            guard kind == UInt8(ascii: "\"") else { throw Invalid() }
            var i = range.lowerBound
            return String(decoding: Scanner.unescape(bytes, &i), as: UTF8.self)
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
        /// invalid: a `String`-keyed result cannot hold both, and Go refuses them too (spec/session.md, wire JSON).
        func stringMap(_ name: String) throws -> [String: String]? {
            guard let o = try object(name) else { return nil }
            var out: [String: String] = [:]
            for (k, n) in o.members {
                let key = String(decoding: k, as: UTF8.self)
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

    /// Helpers that walk a document already validated.
    enum Scanner {
        static func isSpace(_ b: UInt8) -> Bool { b == 0x20 || b == 0x09 || b == 0x0A || b == 0x0D }

        static func skipSpace(_ b: [UInt8], _ i: Int) -> Int {
            var i = i
            while i < b.count, isSpace(b[i]) { i += 1 }
            return i
        }

        /// The end of the value starting at `i`.
        static func skipValue(_ b: [UInt8], _ i: Int) -> Int {
            switch b[i] {
            case UInt8(ascii: "\""):
                var j = i
                _ = unescape(b, &j)
                return j
            case UInt8(ascii: "{"), UInt8(ascii: "["):
                var depth = 0
                var j = i
                while true {
                    switch b[j] {
                    case UInt8(ascii: "\""):
                        _ = unescape(b, &j)
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

        /// The UTF-8 bytes of the string starting at `i` (its opening quote); `i` ends after the closing quote.
        static func unescape(_ b: [UInt8], _ i: inout Int) -> [UInt8] {
            var out: [UInt8] = []
            i += 1
            while b[i] != UInt8(ascii: "\"") {
                guard b[i] == UInt8(ascii: "\\") else {
                    out.append(b[i])
                    i += 1
                    continue
                }
                let e = b[i + 1]
                i += 2
                switch e {
                case UInt8(ascii: "b"): out.append(0x08)
                case UInt8(ascii: "f"): out.append(0x0C)
                case UInt8(ascii: "n"): out.append(0x0A)
                case UInt8(ascii: "r"): out.append(0x0D)
                case UInt8(ascii: "t"): out.append(0x09)
                case UInt8(ascii: "u"):
                    var scalar = hex4(b, i)
                    i += 4
                    if scalar >= 0xD800 && scalar <= 0xDBFF {  // validated: a low surrogate escape follows
                        let low = hex4(b, i + 2)
                        i += 6
                        scalar = 0x10000 + ((scalar - 0xD800) << 10) + (low - 0xDC00)
                    }
                    out += Array(String(Character(Unicode.Scalar(scalar)!)).utf8)
                default: out.append(e)  // " \ /
                }
            }
            i += 1
            return out
        }

        static func hexDigit(_ c: UInt8) -> UInt32? {
            switch c {
            case 0x30...0x39: return UInt32(c - 0x30)
            case 0x41...0x46: return UInt32(c - 0x41 + 10)
            case 0x61...0x66: return UInt32(c - 0x61 + 10)
            default: return nil
            }
        }

        static func hex4(_ b: [UInt8], _ i: Int) -> UInt32 {
            (0..<4).reduce(0) { $0 << 4 | (hexDigit(b[i + $1]) ?? 0) }
        }
    }

    /// Checks a whole document without recursion, so nesting to the depth limit cannot exhaust a thread's stack.
    struct Validator {
        let b: [UInt8]
        var i = 0

        init(_ b: [UInt8]) { self.b = b }

        final class Names {
            var set: Set<[UInt8]>
            init(_ first: [UInt8]) { set = [first] }
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
                                needValue = false
                            } else {
                                stack.append(.object(names: Names(try name())))
                                skipSpace()
                                try expect(":")
                            }
                        } else if peek() == UInt8(ascii: "]") {
                            i += 1
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
                    needValue = false
                    continue
                }
                // A value just ended.
                guard let top = stack.last else {
                    guard i == b.count else { throw Invalid() }  // a second value, or trailing garbage
                    return start..<Scanner.skipValue(b, start)
                }
                guard let c = peek() else { throw Invalid() }
                switch (top, c) {
                case (.object(let names), UInt8(ascii: ",")):
                    i += 1
                    skipSpace()
                    let n = try name()
                    guard names.set.insert(n).inserted else { throw Invalid() }  // a duplicate member name
                    skipSpace()
                    try expect(":")
                    needValue = true
                case (.object, UInt8(ascii: "}")), (.array, UInt8(ascii: "]")):
                    i += 1
                    stack.removeLast()
                case (.array, UInt8(ascii: ",")):
                    i += 1
                    needValue = true
                default:
                    throw Invalid()
                }
            }
        }

        private mutating func name() throws -> [UInt8] {
            guard peek() == UInt8(ascii: "\"") else { throw Invalid() }
            let at = i
            try string()
            var j = at
            return Scanner.unescape(b, &j)
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
