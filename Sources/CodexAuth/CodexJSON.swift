#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// The document `raw` if it can be JSON as Go reads it, read in place: UTF-8 without a byte-order mark, within the
/// depth and number bounds, and strict. JSONSerialization also accepts UTF-16 and UTF-32, whose text always holds
/// NUL bytes, and skips a leading UTF-8 byte-order mark; Go accepts neither, and no JSON text holds a raw NUL. The
/// strict grammar also refuses duplicate keys and invalid UTF-8, which JSONSerialization and Go's encoding/json each
/// settle their own way; Codex's serde refuses both. No check copies any of `raw`.
func plainJSON(_ raw: UnsafeRawBufferPointer) -> WireJSON.Borrowed? {
    guard !raw.contains(0), !(raw.count >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF),
        withinBounds(raw, maxCodexJSONDepth)
    else { return nil }
    return WireJSON.Borrowed(raw)
}

/// How deeply Codex's JSON (a reply, auth.json, a JWT payload) may nest, as in Go (`codexapp.MaxJSONDepth`). Codex's
/// own documents nest a few levels; the bound is one both implementations' decoders accept (Foundation's stops at
/// 512), so a deeper document is malformed in both rather than in one.
let maxCodexJSONDepth = 64

/// Whether no array or object in `bytes` nests deeper than `max` and every number outside strings fits a Double, as
/// in Go (`codexapp.ValidJSON`). Foundation refuses 1e999 wherever it is (and reads -1e400 as -inf), and Codex's
/// serde refuses both, so such a number makes the document malformed in both implementations. It reads `bytes` in
/// place: a number is judged from its digits where they lie.
func withinBounds(_ bytes: UnsafeRawBufferPointer, _ max: Int) -> Bool {
    var depth = 0, inString = false, escaped = false
    var number: Int? = nil  // where the number being read starts
    for (i, c) in bytes.enumerated() {
        if !inString, c == 0x2D || c == 0x2B || c == 0x2E || c == 0x65 || c == 0x45 || (c >= 0x30 && c <= 0x39) {
            if number != nil { continue }
            if c == 0x2D || (c >= 0x30 && c <= 0x39) {
                number = i
                continue
            }
        }
        if let n = number {
            guard finiteAsDouble(UnsafeRawBufferPointer(rebasing: bytes[n..<i])) else { return false }
            number = nil
        }
        if inString {
            if escaped {
                escaped = false
            } else if c == 0x5C {
                escaped = true
            } else if c == 0x22 {
                inString = false
            }
        } else if c == 0x22 {
            inString = true
        } else if c == 0x5B || c == 0x7B {
            depth += 1
            if depth > max { return false }
        } else if c == 0x5D || c == 0x7D {
            depth -= 1
        }
    }
    if let n = number { return finiteAsDouble(UnsafeRawBufferPointer(rebasing: bytes[n...])) }
    return true
}

/// The digits of 2^1024 - 2^970, halfway between the largest finite Double and 2^1024: a number at least this large
/// rounds, to even, up to 2^1024, which is infinite.
private let overflowDigits: [UInt8] = Array("179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792".utf8)

/// Whether the JSON number `n` is finite as a correctly rounded Double (as Go's ParseFloat and Foundation read it),
/// decided from its digits without converting it, so nothing of it is copied. Text that is no JSON number counts as
/// finite: it is the strict check's to refuse.
func finiteAsDouble(_ n: UnsafeRawBufferPointer) -> Bool {
    var i = 0
    if i < n.count, n[i] == 0x2D { i += 1 }
    func digits() -> Range<Int> {
        let s = i
        while i < n.count, n[i] >= 0x30, n[i] <= 0x39 { i += 1 }
        return s..<i
    }
    let whole = digits()
    guard !whole.isEmpty else { return true }
    var fraction = i..<i
    if i < n.count, n[i] == 0x2E {
        i += 1
        fraction = digits()
        guard !fraction.isEmpty else { return true }
    }
    var exponent = 0
    if i < n.count, n[i] == 0x65 || n[i] == 0x45 {
        i += 1
        var negative = false
        if i < n.count, n[i] == 0x2B || n[i] == 0x2D {
            negative = n[i] == 0x2D
            i += 1
        }
        let e = digits()
        guard !e.isEmpty else { return true }
        for k in e { exponent = Swift.min(exponent * 10 + Int(n[k] - 0x30), 1_000_000_000) }  // saturated
        if negative { exponent = -exponent }
    }
    guard i == n.count else { return true }
    // The significant digits run through `whole` then `fraction`; the value is 0.d1d2... × 10^point.
    let all = whole.count + fraction.count
    func digit(_ k: Int) -> UInt8 { k < whole.count ? n[whole.lowerBound + k] : n[fraction.lowerBound + k - whole.count] }
    var first = 0
    while first < all, digit(first) == 0x30 { first += 1 }
    guard first < all else { return true }  // zero
    let point = whole.count - first + exponent
    if point != overflowDigits.count { return point < overflowDigits.count }
    for (k, t) in overflowDigits.enumerated() {
        let d = first + k < all ? digit(first + k) : 0x30
        if d != t { return d < t }
    }
    return false  // at least the halfway point
}

/// A JSON object, with null read as an empty one.
func jsonObject(_ v: Any) -> [String: Any]? {
    if v is NSNull { return [:] }
    return v as? [String: Any]
}

/// `.some(nil)` for absent or null, `.some(s)` for a string, nil for any other type.
func jsonOptionalString(_ v: Any?) -> String?? {
    guard let v, !(v is NSNull) else { return .some(nil) }
    guard let s = v as? String else { return nil }
    return .some(s)
}
#endif
