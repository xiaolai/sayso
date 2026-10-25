import Foundation

typealias SvAlt = [String]  // tokens acceptable at one position
typealias SvSeq = [SvAlt]  // one reading
typealias SvUnit = [SvSeq]  // the alternative readings of one script unit

func lit(_ tokens: [String]) -> SvSeq { tokens.map { [$0] } }
func lit(_ tokens: String...) -> SvSeq { lit(tokens) }
func cat(_ seqs: SvSeq...) -> SvSeq { seqs.flatMap { $0 } }

let svZeroAlt: SvAlt = ["zero", "oh", "o"]

/// A reading of digits as a sequence where every "zero" also accepts "oh" and "o".
func svWordsWithZero(_ words: [String]) -> SvSeq { words.map { $0 == "zero" ? svZeroAlt : [$0] } }

/// Every scalar is an ASCII digit; true for "", as in Go.
func svAllDigits(_ s: String) -> Bool { s.unicodeScalars.allSatisfy(svDigit) }

let svScales: [String: UInt64] = Dictionary(uniqueKeysWithValues: NumWords.scales.map { ($0.name, $0.v) })

func svIsScaleWord(_ w: String) -> Bool { svScales[w] != nil || w == "hundred" }

/// The British "and" before the final part under a hundred ("one hundred and five").
func svWithAnd(_ words: [String]) -> [String]? {
    guard let last = words.lastIndex(where: svIsScaleWord), last != words.count - 1 else { return nil }
    return Array(words[...last]) + ["and"] + Array(words[(last + 1)...])
}

/// The British reading with an "and" in every group: "one million two hundred and thirty four thousand five hundred
/// and sixty seven", "a million and one".
func svWithAllAnds(_ words: [String]) -> [String] {
    var out: [String] = []
    for (i, w) in words.enumerated() {
        out.append(w)
        if w == "hundred" && i + 1 < words.count && !svIsScaleWord(words[i + 1]) { out.append("and") }
    }
    if let last = words.lastIndex(where: svIsScaleWord), words[last] != "hundred", last < words.count - 1 {
        let at = out.count - (words.count - 1 - last)  // the tail under a hundred follows the last scale word
        out = Array(out[..<at]) + ["and"] + Array(out[at...])
    }
    return out
}

func svCommaDigits(_ n: UInt64) -> String { svGroupDigits(String(n)) }

/// A string of digits with thousands commas ("1000000000000000" → "1,000,000,000,000,000").
func svGroupDigits(_ digits: String) -> String {
    let s = Array(digits)
    var out = ""
    for (i, c) in s.enumerated() {
        if i > 0 && (s.count - i) % 3 == 0 { out.append(",") }
        out.append(c)
    }
    return out
}

/// The word readings of n, with no "and", one, or one per group, and with "a" for a leading one before a scale word
/// ("a hundred and sixty").
func svCardinalForms(_ n: UInt64) -> [[String]] {
    let words = NumWords.cardinal(n)
    var forms = [words]
    var seen: Set<String> = [words.joined(separator: " ")]
    func add(_ f: [String]?) {
        guard let f, seen.insert(f.joined(separator: " ")).inserted else { return }
        forms.append(f)
    }
    add(svWithAnd(words))
    add(svWithAllAnds(words))
    for f in forms where f.count > 1 && f[0] == "one" && svIsScaleWord(f[1]) {
        add(["a"] + f.dropFirst())
    }
    return forms
}

func svYearReadings(_ n: UInt64) -> [SvSeq] {
    if n < 1000 || n > 2099 { return [] }
    let hi = n / 100
    let lo = n % 100
    if lo == 0 && hi % 10 != 0 { return [lit(NumWords.cardinal(hi) + ["hundred"])] }
    if lo == 0 { return [] }
    if lo < 10 { return [cat(lit(NumWords.cardinal(hi)), [svZeroAlt], lit(NumWords.cardinal(lo)))] }
    return [lit(NumWords.cardinal(hi) + NumWords.cardinal(lo))]
}

func svOrdinalSuffix(_ n: UInt64) -> String {
    if n % 100 >= 11 && n % 100 <= 13 { return "th" }
    switch n % 10 {
    case 1: return "st"
    case 2: return "nd"
    case 3: return "rd"
    default: return "th"
    }
}

/// A comma-separated list of plain numbers in a language whose comma groups digits ("1,2,3").
func svCommaList(_ text: String, _ language: String) -> Bool {
    let b = baseLanguage(language)
    if (b != "en" && b != "zh") || !text.contains(",") || svIsGrouped(text) { return false }
    return split(text, ",").allSatisfy { !$0.isEmpty && svAllDigits($0) }
}

/// "1,000" and "12,345,678", digits only, groups of three after a first group of one to three.
func svIsGrouped(_ s: String) -> Bool {
    let parts = split(s, ",")
    guard parts.count >= 2, let first = parts.first, (1...3).contains(first.utf8.count), first.utf8.first != 0x30
    else { return false }
    for (i, p) in parts.enumerated() where (i > 0 && p.utf8.count != 3) || !svAllDigits(p) { return false }
    return true
}

/// The readings of a number written in the script. Every reading says its whole value: sign, magnitude, fraction,
/// ordinal suffix and percent, so no alternative can drop one of them. A comma list ("1,2,3") is several numbers.
func svNumberUnits(_ text: String, _ language: String, _ spell: SpellOut?) -> [SvUnit] {
    let low = svLower(text, language)
    let base = baseLanguage(language)
    var body = low
    var sign = ""
    if body.hasPrefix("-") {
        body = String(body.dropFirst())
        sign = "-"
    }
    let percent = body.hasSuffix("%")
    if percent { body = String(body.dropLast()) }
    var suffix = ""
    let b = Array(body.utf8)
    if b.count > 2 && svDigit(Unicode.Scalar(b[b.count - 3])) {
        let s = String(decoding: b[(b.count - 2)...], as: UTF8.self)
        if s == "st" || s == "nd" || s == "rd" || s == "th" {
            body = String(decoding: b[..<(b.count - 2)], as: UTF8.self)
            suffix = s
        }
    }
    let percentAlt: SvAlt = base == "en" ? ["%", "percent"] : ["%"]
    func finish(_ u: SvUnit) -> [SvUnit] { [percent ? u.map { cat($0, [percentAlt]) } : u] }
    func literal() -> [SvUnit] { finish([lit(sign + body + suffix)]) }
    if base != "en" && base != "zh" || containsAny(body, "/:") {
        // Digits match digits; in a spell-out language the locale's spelling of the value is a reading too.
        guard let spell, suffix.isEmpty, !containsAny(body, "/:"), let spelled = spell.spelled(digits: body, sign: sign)
        else { return literal() }
        return finish([lit(sign + body + suffix), lit(spelled)])
    }
    var intPart = body
    var frac = ""
    if let dot = body.lastIndex(of: ".") {
        intPart = String(body[..<dot])
        frac = String(body[body.index(after: dot)...])
        if frac.isEmpty || !svAllDigits(frac) { return literal() }
    }
    var grouped = false
    if intPart.contains(",") {
        if svIsGrouped(intPart) {
            intPart = intPart.replacingOccurrences(of: ",", with: "")
            grouped = true
        } else if frac.isEmpty && sign.isEmpty && suffix.isEmpty {
            // A list ("1,2,3"; "1,2,3%" ends in a percentage, as the transcript side cuts it).
            let parts = split(intPart, ",")
            var out: [SvUnit] = []
            for (i, p) in parts.enumerated() {
                if p.isEmpty || !svAllDigits(p) { return literal() }
                out += svNumberUnits(i == parts.count - 1 && percent ? p + "%" : p, language, spell)
            }
            return out
        } else {
            return literal()
        }
    }
    if !svAllDigits(intPart) || (intPart.isEmpty && (frac.isEmpty || !suffix.isEmpty)) || (!frac.isEmpty && !suffix.isEmpty) {
        return literal()
    }
    let fracText = frac.isEmpty ? "" : "." + frac
    let leadingZero = intPart.utf8.count > 1 && intPart.utf8.first == 0x30
    let parsed = UInt64(intPart)
    let n = parsed ?? 0
    let cardinal = !intPart.isEmpty && !leadingZero && parsed != nil && n <= NumWords.maxCardinal
    if !suffix.isEmpty && (!cardinal || svOrdinalSuffix(n) != suffix) {
        return literal()  // "2st", "007th": written as given, no spoken alternative
    }

    var unit: SvUnit = [lit(sign + body + suffix)]  // as written ("1,000,000,000,000,000")
    var digitForms = [intPart + fracText]
    if intPart.isEmpty {
        digitForms.append("0" + fracText)
    } else if !leadingZero && intPart.utf8.count >= 4 {
        digitForms.append(svGroupDigits(intPart) + fracText)  // also beyond the cardinal range
    }
    for d in digitForms {
        let written = lit(sign + d + suffix)
        if !unit.contains(written) { unit.append(written) }  // "42" and "1,000" are already the reading as written
        if !sign.isEmpty && base == "en" {
            unit.append(lit("minus", d + suffix))
            unit.append(lit("negative", d + suffix))
        }
    }
    if base == "zh" {
        // The Han spellings reach further than the English cardinals: up to svHanMax (一千万亿 and beyond).
        if !intPart.isEmpty && !leadingZero && parsed != nil && n <= svHanMax && sign.isEmpty && suffix.isEmpty
            && frac.isEmpty
        {
            unit += svHanSeqs(n)
        }
        return finish(unit)
    }

    var words: [SvSeq] = []
    let fracWords = svWordsWithZero(NumWords.digits(frac))
    if intPart.isEmpty {  // ".5"
        words.append(cat(lit("point"), fracWords))
        words.append(cat([svZeroAlt], lit("point"), fracWords))
    } else if !cardinal && grouped {
        // beyond the cardinal range a grouped number is as written, never digit by digit
    } else if !cardinal {  // "007", "01.5", more than fifteen digits: digit by digit
        var w = svWordsWithZero(NumWords.digits(intPart))
        if !frac.isEmpty { w = cat(w, lit("point"), fracWords) }
        words.append(w)
    } else if !frac.isEmpty {
        for f in svCardinalForms(n) { words.append(cat(lit(f), lit("point"), fracWords)) }
        if intPart.utf8.count >= 4 && !grouped {
            words.append(cat(svWordsWithZero(NumWords.digits(intPart)), lit("point"), fracWords))  // "1234.5"
        }
    } else if !suffix.isEmpty {
        for f in svCardinalForms(n) { words.append(lit(NumWords.ordinal(f))) }
    } else {
        for f in svCardinalForms(n) { words.append(lit(f)) }
        if intPart.utf8.count == 4 && !grouped { words += svYearReadings(n) }
        if intPart.utf8.count >= 4 && !grouped {
            words.append(svWordsWithZero(NumWords.digits(intPart)))  // a code or phone number
        }
    }
    for w in words {
        if !sign.isEmpty {
            unit.append(cat(lit("minus"), w))
            unit.append(cat(lit("negative"), w))
        } else {
            unit.append(w)
        }
    }
    return finish(unit)
}

func svSplitRunes(_ s: String) -> [String] { s.unicodeScalars.map { String($0) } }

// --- Han numerals ---

let svHanDigit: [Unicode.Scalar: UInt64] = [
    "〇": 0, "零": 0, "一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9,
]
let svHanSmallUnit: [Unicode.Scalar: UInt64] = ["十": 10, "百": 100, "千": 1000]
let svHanBigUnit: [Unicode.Scalar: UInt64] = ["万": 10_000, "亿": 100_000_000]

/// Reads a Han numeral strictly: it computes a value, then accepts the run only if the run is one of the spellings of
/// that value (svHanSeqs: standard, 两 before a unit, 一十, 〇 for 零, and the colloquial dropped final unit 一百一 = 110,
/// 两万五 = 25000). Repeated or out-of-order units, a leading 零, a 零 that skips no place, and anything that overflows
/// fail the round trip by construction.
func svParseHan(_ run: [Unicode.Scalar]) -> UInt64? {
    guard let n = svHanValue(run) else { return nil }
    let text = run.map { String($0) }
    for seq in svHanSeqs(n) where seq.count == run.count {
        if zip(seq, text).allSatisfy({ $0.contains($1) }) { return n }
    }
    return nil
}

/// The value a well-formed run would have. It does not validate; svParseHan does.
func svHanValue(_ run: [Unicode.Scalar]) -> UInt64? {
    var total: UInt64 = 0
    var section: UInt64 = 0
    var pending = -1
    var zero = false
    var any = false
    var zeroInSection = false
    var lastUnit: UInt64 = 0
    for (i, r) in run.enumerated() {
        if let d = svHanDigit[r] {
            if d == 0 {
                zero = true
                continue
            }
            if pending >= 0 { return nil }
            if zero {
                zero = false
                zeroInSection = true
            }
            pending = Int(d)
            any = true
            continue
        }
        if let u = svHanSmallUnit[r] {
            if pending >= 0 {
                section += UInt64(pending) * u
            } else if u == 10 && i == 0 {
                section += 10
            } else {
                return nil
            }
            pending = -1
            lastUnit = u
            any = true
            // A zero before this unit skipped a higher place (一万零一百五); only a zero after the last unit makes the
            // final digit a units digit (一百零五).
            zeroInSection = false
            continue
        }
        var group = section
        if pending >= 0 { group += UInt64(pending) }
        switch r {
        case "万":
            if group == 0 || group >= 10_000 || total >= 100_000_000_000_000_000 { return nil }
            total += group * 10_000
            lastUnit = 10_000
        case "亿":
            if total + group == 0 || total + group >= 10_000_000_000 { return nil }
            total = (total + group) * 100_000_000
            lastUnit = 100_000_000
        default:
            return nil
        }
        section = 0
        pending = -1
        zeroInSection = false
    }
    if pending >= 0 {
        if lastUnit >= 100 && section % 10 == 0 && !zeroInSection {  // 一百一 = 110, 两万五 = 25000, but 一百零一 = 101
            section += UInt64(pending) * (lastUnit / 10)
        } else {
            section += UInt64(pending)
        }
    }
    return any ? total + section : nil
}

private let svHanNumerals: [String] = ["零", "一", "二", "三", "四", "五", "六", "七", "八", "九"]

/// The largest value svRenderHan writes: its 亿 group stays below 一万万.
let svHanMax: UInt64 = 9_999_999_999_999_999

func svRenderSection(_ n: UInt64, _ leadingTen: Bool) -> String {
    var out = ""
    let units: [(v: UInt64, s: String)] = [(1000, "千"), (100, "百"), (10, "十"), (1, "")]
    var started = false
    var zeroPending = false
    for u in units {
        let d = n / u.v % 10
        if d == 0 {
            if started { zeroPending = true }
            continue
        }
        if zeroPending {
            out += "零"
            zeroPending = false
        }
        if !(u.v == 10 && d == 1 && !started && leadingTen) { out += svHanNumerals[Int(d)] }
        out += u.s
        started = true
    }
    return out
}

/// n in standard Han numerals (110 = 一百一十, 101 = 一百零一, 10 = 十, 1e12 = 一万亿). With `short` false a leading ten is
/// written 一十.
func svRenderHan(_ n: UInt64, _ short: Bool) -> String {
    if n == 0 { return "零" }
    let yi = n / 100_000_000
    let wan = n / 10_000 % 10_000
    let rest = n % 10_000
    var out = ""
    if yi > 0 {
        if yi >= 10_000 {
            out += svRenderSection(yi / 10_000, short) + "万"
            if yi % 10_000 > 0 {
                if yi % 10_000 < 1000 { out += "零" }
                out += svRenderSection(yi % 10_000, false)
            }
        } else {
            out += svRenderSection(yi, short)
        }
        out += "亿"
    }
    if wan > 0 {
        if yi > 0 && wan < 1000 { out += "零" }
        out += svRenderSection(wan, short && yi == 0) + "万"
    }
    if rest > 0 {
        if ((yi > 0 || wan > 0) && rest < 1000) || (yi > 0 && wan == 0) { out += "零" }  // 一亿零一千: the empty 万 group is a gap
        out += svRenderSection(rest, short && yi == 0 && wan == 0)
    }
    return out
}

private let svNextHigher: [Unicode.Scalar: Unicode.Scalar] = ["十": "百", "百": "千", "千": "万"]

/// Drops a final unit that is one place below the unit before its digit: 一百一十 → 一百一, 两万五千 → 两万五. nil when the
/// form has none.
func svHanColloquial(_ f: String) -> String? {
    let rs = Array(f.unicodeScalars)
    let k = rs.count
    if k < 4 { return nil }
    let u = rs[k - 1]
    let d = rs[k - 2]
    let v = rs[k - 3]
    guard svHanSmallUnit[u] != nil, (svHanDigit[d] ?? 0) != 0, svNextHigher[u] == v else { return nil }
    return svString(rs[..<(k - 1)])
}

/// Every accepted spelling of n, as sequences whose positions allow 两 for 二 before a unit and 〇 for 零.
func svHanSeqs(_ n: UInt64) -> [SvSeq] {
    var forms = [svRenderHan(n, true)]
    let long = svRenderHan(n, false)
    if long != forms[0] { forms.append(long) }
    for f in forms {
        if let c = svHanColloquial(f) { forms.append(c) }
    }
    var out: [SvSeq] = []
    for f in forms {
        let rs = Array(f.unicodeScalars)
        var seq: SvSeq = []
        for (i, r) in rs.enumerated() {
            // 两 is a whole coefficient: before 百 or 千, or before 万 or 亿 when nothing of that group precedes it
            // (两万, 一亿两千万; not 十两万 for 十二万).
            if r == "二" && i + 1 < rs.count
                && ("百千".unicodeScalars.contains(rs[i + 1])
                    || ("万亿".unicodeScalars.contains(rs[i + 1]) && (i == 0 || "万亿零".unicodeScalars.contains(rs[i - 1]))))
            {
                seq.append(["二", "两"])
            } else if r == "零" {
                seq.append(["零", "〇"])
            } else {
                seq.append([String(r)])
            }
        }
        out.append(seq)
    }
    if n == 2 { out.append(lit("两")) }
    return out
}

func svHanUnit(_ run: String) -> SvUnit {
    var unit: SvUnit = [lit(svSplitRunes(run))]
    let rs = Array(run.unicodeScalars)
    let hasUnit = rs.contains { svHanDigit[$0] == nil }
    if !hasUnit {
        // A digit string (二〇二四). Two digits are an approximation (两三天, 三五天: two or three days), and 两 is never a
        // digit of a string, so neither has a reading in digits.
        if rs.count == 2 || (rs.count > 2 && rs.contains("两")) { return unit }
        unit.append(lit(svString(rs.map { Unicode.Scalar(UInt8(0x30 + svHanDigit[$0]!)) })))
        // A lone digit is a number: its other spellings (二 and 两, 零 and 〇).
        if rs.count == 1 {
            for seq in svHanSeqs(UInt64(svHanDigit[rs[0]]!)) where !unit.contains(seq) { unit.append(seq) }
        }
        return unit
    }
    guard let n = svParseHan(rs) else { return unit }
    unit.append(lit(String(n)))
    if n >= 1000 { unit.append(lit(svCommaDigits(n))) }
    for seq in svHanSeqs(n) where !unit.contains(seq) { unit.append(seq) }  // the run as written is one of them
    return unit
}

// --- English number words in the script ---

func svNumberWordValue(_ w: String) -> UInt64? { NumWords.smallValue(w).map { UInt64($0) } }

/// Reads a cardinal written in words, strictly: "one hundred and five", "a hundred and sixty", "two thousand",
/// "a million and one". "zero" is a cardinal only alone ("one thousand zero" is not one number). Digit-by-digit
/// readings ("three seven oh four") are not cardinals.
func svParseEnglish(_ words: ArraySlice<String>) -> UInt64? {
    var total: UInt64 = 0
    var group: UInt64 = 0
    var lastScale: UInt64 = 1_000_000_000_000_000
    var prevSmall = false
    var prevTens = false
    var sawNumber = false
    for i in words.indices {
        let w = words[i]
        if w == "and" {  // between a scale word and a number under it ("hundred and five", not "hundred and thousand")
            if i == words.startIndex || i == words.endIndex - 1 || !svIsScaleWord(words[i - 1]) || svIsScaleWord(words[i + 1])
                || words[i + 1] == "and"
            {
                return nil
            }
        } else if w == "a" {
            if i + 1 >= words.endIndex || !svIsScaleWord(words[i + 1]) || prevSmall || group != 0 { return nil }
            group = 1
            prevSmall = true
            prevTens = false
            sawNumber = true
        } else if w == "zero" {
            if words.count != 1 { return nil }
            sawNumber = true
        } else if w == "hundred" {
            if group == 0 || group > 9 || !prevSmall { return nil }
            group *= 100
            prevSmall = false
            prevTens = false
        } else if let s = svScales[w] {
            if group == 0 || s >= lastScale { return nil }
            total += group * s
            group = 0
            lastScale = s
            prevSmall = false
            prevTens = false
        } else {
            guard let v = svNumberWordValue(w) else { return nil }
            if prevSmall && !(prevTens && v > 0 && v < 10) { return nil }
            if !prevSmall && group % 100 != 0 { return nil }
            group += v
            prevSmall = true
            prevTens = v >= 20 && v % 10 == 0
            sawNumber = true
        }
    }
    return sawNumber ? total + group : nil
}

func svIsEnglishNumberWord(_ w: String) -> Bool { svNumberWordValue(w) != nil || svIsScaleWord(w) }

/// An ordinal word mapped to the cardinal word it ends ("first" → "one", "twentieth" → "twenty").
let svOrdinalWords: [String: String] = {
    var m: [String: String] = [:]
    for v in UInt64(1)..<100 where v < 20 || v % 10 == 0 {
        let c = NumWords.cardinal(v)
        m[NumWords.ordinal(c)[0]] = c[0]
    }
    for s in ["hundred"] + NumWords.scales.map(\.name) { m[NumWords.ordinal([s])[0]] = s }
    m["zeroth"] = "zero"
    return m
}()

/// Longer than any cardinal up to NumWords.maxCardinal, so parsing stays linear.
let svMaxPhraseWords = 48

func svPhraseWord(_ w: String) -> Bool {
    svIsEnglishNumberWord(w) || svOrdinalWords[w] != nil || w == "and" || w == "a" || w == "point" || w == "percent"
        || w == "minus" || w == "negative"
}

/// Reads a number written in words, [minus] cardinal [point digits] [percent], or a cardinal ending in an ordinal
/// word, and returns it written in digits ("-3.5%", "21st").
func svParseEnglishPhrase(_ words: ArraySlice<String>) -> String? {
    var i = words.startIndex
    var sign = ""
    if let first = words.first, first == "minus" || first == "negative" {
        i += 1
        sign = "-"
    }
    var end = words.endIndex
    var percent = false
    if end > i && words[end - 1] == "percent" {
        end -= 1
        percent = true
    }
    if end < i { return nil }
    var card = words[i..<end]
    var frac = ""
    if let p = card.firstIndex(of: "point") {
        let digits = card[(p + 1)...]
        if digits.isEmpty { return nil }
        for d in digits {
            guard let v = NumWords.smallValue(d), v <= 9 else { return nil }
            frac.append(Character(Unicode.Scalar(UInt8(0x30 + v))))
        }
        card = card[..<p]
    }
    if card.isEmpty { return nil }
    var ordinal = false
    if let c = svOrdinalWords[card[card.endIndex - 1]] {
        if !frac.isEmpty || percent || !sign.isEmpty { return nil }
        card = card.dropLast() + [c]
        ordinal = true
    }
    guard let n = svParseEnglish(card) else { return nil }
    var out = sign + String(n)
    if !frac.isEmpty { out += "." + frac }
    if ordinal { out += svOrdinalSuffix(n) }
    if percent { out += "%" }
    return out
}
