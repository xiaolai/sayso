import Foundation

// The said guard answers one question about a text-to-speech take: did the model read exactly this script?
//
// It generates the correct readings of the script (numbers, symbols, abbreviations, spelled codes, Han numerals,
// and, in the languages of `saidGuardSpellOutLanguages`, the locale's spell-out) and matches the model's own
// transcript against them, one reading per script unit, in order. The transcript is only normalised and tokenised,
// never parsed back into numbers or abbreviations, and it deletes no spoken character. The contract is
// spec/said-guard.md; the cases are fixtures/said-guard.json. This is a port of the Go package `saidguard`; the
// names of its functions are kept so the two can be read side by side.
//
// Three invariants hold by construction rather than by fixture:
//   - A number's every reading carries all of its sign, fraction, ordinal suffix and percent (svNumberUnits).
//   - A Han numeral is a number only if it is one of the spellings of its own value (svParseHan round-trips).
//   - Matching is a bitset simulation over transcript positions, linear in the frontier it actually carries.
// The spell-out adds a fourth: a spelled reading is the locale's own spelling of a value parsed strictly from the
// script, and a script number word is a number only if it is the locale's spelling of that number.

/// Bounds each side; a longer script or transcript never matches.
public let saidGuardMaxBytes = 64 << 10

/// The said guard's revision (spec/said-guard.md), raised whenever a change makes the guard refuse a reading it
/// accepted. A cache of audio accepted by the guard keys on it, so audio accepted by an older, looser guard is not
/// reused. 5: minus signs and operators kept in more contexts, paired quotation marks, the a/I rule in every language.
public let saidGuardVersion = 7

/// The base languages whose numbers also have the locale's spell-out as a reading (the `spellout:<lang>`
/// capabilities of spec/parity.md). `en` and `zh` use the guard's own tables.
public let saidGuardSpellOutLanguages: Set<String> = ["ar", "de", "es", "fr", "hi", "it", "ja", "ko", "pt"]

/// Whether the model's own transcript (`said`) is a reading of the script (`spoken`) in `language`, a BCP 47 tag
/// ("en", "zh-Hans"). An empty script never matches.
public func saidMatches(_ said: String, _ spoken: String, language: String) -> Bool {
    if said.utf8.count > saidGuardMaxBytes || spoken.utf8.count > saidGuardMaxBytes || svOverlongMarks(said)
        || svOverlongMarks(spoken)
    {
        return false
    }
    let units = svScriptUnits(spoken, language)
    if units.isEmpty { return false }
    return svMatch(units, svTranscriptTokens(said, language))
}

/// Reads `s` as a Chinese numeral written in characters (一百一十, 两万五, 零), or as a digit string of one digit or
/// three or more (一〇八, 二〇二四); nil for a run that is not a well-formed numeral (一五一十, 亿万, 零十), a two-digit
/// string (三五 is "three or five", an approximation), a digit string with 两, or a value that overflows. Units are
/// read by the strict parser the said guard uses.
public func parseHanNumeral(_ s: String) -> UInt64? {
    let rs = Array(s.unicodeScalars)
    guard !rs.isEmpty, rs.allSatisfy({ svHanDigit[$0] != nil }) else { return svParseHan(rs) }
    if rs.count == 2 || (rs.count > 2 && rs.contains("两")) { return nil }
    var n: UInt64 = 0
    for r in rs {
        let (m, o1) = n.multipliedReportingOverflow(by: 10)
        let (v, o2) = m.addingReportingOverflow(svHanDigit[r]!)
        if o1 || o2 { return nil }
        n = v
    }
    return n
}

// --- normalisation shared by both sides ---

/// Bounds a run of combining marks. Normalisers differ beyond 30 (Go's inserts U+034F to keep the text stream-safe,
/// Foundation's does not), so a side with a longer run never matches; a lowercase mapping can add one mark, hence 29.
let svMaxMarkRun = 29

/// A run of more than svMaxMarkRun non-starters (canonical combining class above zero, or a Hangul medial or final
/// jamo) in the compatibility decomposition of `s`, or of `s` after the deletions of preNormalize, which can join two
/// runs.
func svOverlongMarks(_ s: String) -> Bool {
    svMarkRun(s) || svMarkRun(preNormalize(nfkc(svSuperscripts(s))))
}

private func svMarkRun(_ s: String) -> Bool {
    if isASCII(s) { return false }
    var run = 0
    for r in s.unicodeScalars {
        let d = r.value < 0x80 ? [r] : Array(String(r).decomposedStringWithCompatibilityMapping.unicodeScalars)
        for x in d {
            if x.properties.canonicalCombiningClass.rawValue != 0 || (0x1160...0x11FF).contains(x.value)
                || (0xD7B0...0xD7FF).contains(x.value)
            {
                run += 1
                if run > svMaxMarkRun { return true }
            } else {
                run = 0
            }
        }
    }
    return false
}

func svString(_ rs: some Sequence<Unicode.Scalar>) -> String {
    var v = String.UnicodeScalarView()
    v.append(contentsOf: rs)
    return String(v)
}

/// NFKC; ASCII is its own normal form, and skipping Foundation for it keeps per-token normalisation cheap.
func nfkc(_ s: String) -> String { isASCII(s) ? s : s.precomposedStringWithCompatibilityMapping }

func isASCII(_ s: String) -> Bool { s.utf8.allSatisfy { $0 < 0x80 } }

// Go applies width.Fold after NFKC; measured over every scalar, it changes nothing NFKC has produced, so it is
// omitted here.
func svPre(_ s: String) -> String { nfkc(preNormalize(nfkc(svSuperscripts(s)))) }

private let svSuperscriptDigits: [Unicode.Scalar] = Array("⁰¹²³⁴⁵⁶⁷⁸⁹".unicodeScalars)

/// A superscript digit is written "^" and the digit, before NFKC would fold it into the number before it ("2³" is
/// two cubed, not twenty-three).
func svSuperscripts(_ s: String) -> String {
    guard s.unicodeScalars.contains(where: svSuperscriptDigits.contains) else { return s }
    var out = String.UnicodeScalarView()
    var prev = false
    let rs = Array(s.unicodeScalars)
    for (k, r) in rs.enumerated() {
        // A superscript sign before superscript digits belongs to the exponent ("2⁻³" is 2^-3); left in place it
        // would fold to a minus before the caret and could be dropped.
        if (r == "⁻" || r == "⁺") && !prev && k + 1 < rs.count && svSuperscriptDigits.contains(rs[k + 1]) {
            out.append(contentsOf: " ^".unicodeScalars)
            out.append(r == "⁻" ? "−" : "+")
            prev = true
            continue
        }
        if let i = svSuperscriptDigits.firstIndex(of: r) {
            if !prev { out.append(contentsOf: " ^".unicodeScalars) }
            out.append(Unicode.Scalar(UInt8(0x30 + i)))
            prev = true
            continue
        }
        prev = false
        out.append(r)
    }
    return String(out)
}

func svLower(_ s: String, _ language: String) -> String {
    switch baseLanguage(language) {
    case "tr", "az": return nfkc(goLower(s, turkish: true))
    default: return nfkc(goLower(s, turkish: false))
    }
}

/// Go's strings.ToLower: the simple per-scalar mapping (İ is i, not i + U+0307), and with `turkish` Go's
/// unicode.TurkishCase (I is ı, İ is i).
func goLower(_ s: String, turkish: Bool = false) -> String {
    if !turkish && isASCII(s) { return s.lowercased() }
    var out = String.UnicodeScalarView()
    for r in s.unicodeScalars {
        if turkish && r == "I" {
            out.append("\u{0131}")
        } else if r == "\u{0130}" {
            out.append("i")
        } else {
            let m = r.properties.lowercaseMapping.unicodeScalars
            if m.count == 1 { out.append(m.first!) } else { out.append(r) }
        }
    }
    return String(out)
}

/// Go's strings.ToUpper: the simple per-scalar mapping (ß stays ß).
func goUpper(_ s: String) -> String {
    var out = String.UnicodeScalarView()
    for r in s.unicodeScalars {
        let u = r.properties.uppercaseMapping.unicodeScalars
        if u.count == 1 {
            out.append(u.first!)
            continue
        }
        let t = r.properties.titlecaseMapping.unicodeScalars
        if t.count == 1 { out.append(t.first!) } else { out.append(r) }
    }
    return String(out)
}

// --- character classes, as Go's unicode package defines them ---

func isLetter(_ r: Unicode.Scalar) -> Bool {
    switch r.properties.generalCategory {
    case .uppercaseLetter, .lowercaseLetter, .titlecaseLetter, .modifierLetter, .otherLetter: return true
    default: return false
    }
}

func isUpper(_ r: Unicode.Scalar) -> Bool { r.properties.generalCategory == .uppercaseLetter }

func isMark(_ r: Unicode.Scalar) -> Bool {
    switch r.properties.generalCategory {
    case .nonspacingMark, .spacingMark, .enclosingMark: return true
    default: return false
    }
}

func isNd(_ r: Unicode.Scalar) -> Bool { r.properties.generalCategory == .decimalNumber }

func isSpace(_ r: Unicode.Scalar) -> Bool { r.properties.isWhitespace }

func isControl(_ r: Unicode.Scalar) -> Bool { r.value < 0x20 || (r.value >= 0x7F && r.value < 0xA0) }

/// Go's strings.TrimSpace.
func trimSpace(_ s: String) -> String {
    let rs = Array(s.unicodeScalars)
    var i = 0
    var j = rs.count
    while i < j && isSpace(rs[i]) { i += 1 }
    while j > i && isSpace(rs[j - 1]) { j -= 1 }
    return svString(rs[i..<j])
}

/// Go's strings.FieldsFunc: the runs between scalars for which `sep` holds, empty runs dropped.
func fieldsFunc(_ s: String, _ sep: (Unicode.Scalar) -> Bool) -> [String] {
    var out: [String] = []
    var cur = String.UnicodeScalarView()
    for r in s.unicodeScalars {
        if sep(r) {
            if !cur.isEmpty { out.append(String(cur)) }
            cur = String.UnicodeScalarView()
        } else {
            cur.append(r)
        }
    }
    if !cur.isEmpty { out.append(String(cur)) }
    return out
}

/// Go's strings.ContainsAny.
func containsAny(_ s: String, _ chars: String) -> Bool {
    let set = Set(chars.unicodeScalars)
    return s.unicodeScalars.contains(where: set.contains)
}

/// Go's strings.Split for a single-scalar separator: "" splits into [""].
func split(_ s: String, _ sep: Unicode.Scalar) -> [String] {
    var out: [String] = []
    var cur = String.UnicodeScalarView()
    for r in s.unicodeScalars {
        if r == sep {
            out.append(String(cur))
            cur = String.UnicodeScalarView()
        } else {
            cur.append(r)
        }
    }
    out.append(String(cur))
    return out
}

/// Format characters, enclosing marks and variation selectors go; a zero width space is a word break; every Unicode
/// decimal digit becomes ASCII at scalar level; ⁄ becomes /. The minus sign − stays: it is always spoken (a sign or an
/// operator), where "-" may also join words, so folding it into "-" let "x−y" read as "xy".
func preNormalize(_ s: String) -> String {
    var out = String.UnicodeScalarView()
    for r in s.unicodeScalars {
        let p = r.properties
        if r == "\u{200B}" {
            out.append(" ")  // zero width space marks a word break (UAX #14 ZW); deleting it glued "now"+"here"
        } else if p.generalCategory == .format || p.generalCategory == .enclosingMark || p.isVariationSelector {
            continue
        } else if r == "⁄" {
            out.append("/")
        } else if p.generalCategory == .decimalNumber && !svDigit(r), let v = p.numericValue {
            out.append(Unicode.Scalar(UInt8(0x30 + Int(v) % 10)))
        } else {
            out.append(r)
        }
    }
    return String(out)
}

/// The scalars of the scripts written without spaces between words (Han, Hiragana, Katakana, Thai, Lao, Khmer,
/// Myanmar), so each character is its own token. Generated from Go's unicode tables (Unicode 17.0.0), so both
/// implementations draw the same line.
private let unspacedRanges: [ClosedRange<UInt32>] = [
    0xE01...0xE3A, 0xE40...0xE5B, 0xE81...0xE82, 0xE84...0xE84, 0xE86...0xE8A, 0xE8C...0xEA3, 0xEA5...0xEA5,
    0xEA7...0xEBD, 0xEC0...0xEC4, 0xEC6...0xEC6, 0xEC8...0xECE, 0xED0...0xED9, 0xEDC...0xEDF, 0x1000...0x109F,
    0x1780...0x17DD, 0x17E0...0x17E9, 0x17F0...0x17F9, 0x19E0...0x19FF, 0x2E80...0x2E99, 0x2E9B...0x2EF3,
    0x2F00...0x2FD5, 0x3005...0x3005, 0x3007...0x3007, 0x3021...0x3029, 0x3038...0x303B, 0x3041...0x3096,
    0x309D...0x309F, 0x30A1...0x30FA, 0x30FD...0x30FF, 0x31F0...0x31FF, 0x32D0...0x32FE, 0x3300...0x3357,
    0x3400...0x4DBF, 0x4E00...0x9FFF, 0xA9E0...0xA9FE, 0xAA60...0xAA7F, 0xF900...0xFA6D, 0xFA70...0xFAD9,
    0xFF66...0xFF6F, 0xFF71...0xFF9D, 0x116D0...0x116E3, 0x16FE2...0x16FE3, 0x16FF0...0x16FF6, 0x1AFF0...0x1AFF3,
    0x1AFF5...0x1AFFB, 0x1AFFD...0x1AFFE, 0x1B000...0x1B122, 0x1B132...0x1B132, 0x1B150...0x1B152, 0x1B155...0x1B155,
    0x1B164...0x1B167, 0x1F200...0x1F200, 0x20000...0x2A6DF, 0x2A700...0x2B81D, 0x2B820...0x2CEAD, 0x2CEB0...0x2EBE0,
    0x2EBF0...0x2EE5D, 0x2F800...0x2FA1D, 0x30000...0x3134A, 0x31350...0x33479,
]

/// A script written without spaces between words, so each character is its own token.
func isUnspaced(_ r: Unicode.Scalar) -> Bool {
    let v = r.value
    if v < 0xE01 { return false }
    var lo = 0
    var hi = unspacedRanges.count - 1
    while lo <= hi {
        let mid = (lo + hi) / 2
        let range = unspacedRanges[mid]
        if v < range.lowerBound {
            hi = mid - 1
        } else if v > range.upperBound {
            lo = mid + 1
        } else {
            return true
        }
    }
    return false
}

/// The primary subtag of a BCP 47 tag, lower case ("zh-Hans" → "zh").
func baseLanguage(_ tag: String) -> String {
    let t = goLower(tag)
    if let i = t.firstIndex(where: { $0 == "-" || $0 == "_" }) { return String(t[..<i]) }
    return t
}

func isApostrophe(_ r: Unicode.Scalar) -> Bool { r == "'" || r == "’" || r == "ʼ" || r == "‘" }

func svDigit(_ r: Unicode.Scalar) -> Bool { r.value >= 0x30 && r.value <= 0x39 }

func svLetter(_ r: Unicode.Scalar) -> Bool { isLetter(r) && !isUnspaced(r) }

/// The hyphens that join a word ("e-mail"). Dashes (– —) separate words and are not joiners.
func svJoiner(_ r: Unicode.Scalar) -> Bool { r == "-" || r == "‐" || r == "‑" }

func svNormApostrophes(_ s: String) -> String {
    svString(s.unicodeScalars.map { isApostrophe($0) ? "'" : $0 })
}
