import Foundation

// --- units ---

private let svTitles = ["Mr": "mister", "Mrs": "missus", "Ms": "miz", "Dr": "doctor"]
private let svSuffixTitles = ["Jr": "junior", "Sr": "senior"]
private let svTitleAbbrev = ["mister": "mr", "missus": "mrs", "doctor": "dr", "saint": "st"]
private let svCaseFree: [String: [String]] = [
    "vs": ["versus"], "etc": ["et", "cetera"], "e.g": ["for", "example"], "i.e": ["that", "is"],
]
private let svAbbreviated = ["for example": "e.g", "et cetera": "etc", "that is": "i.e"]

/// Letters of a spelled code are separated by spaces or initials' dots ("U.S.A."), nothing else.
func svSpelledGap(_ gap: String) -> Bool { trimSpace(gap.replacingOccurrences(of: ".", with: "")).isEmpty }

func word(_ raws: [SvRaw], _ k: Int) -> Bool { k >= 0 && k < raws.count && raws[k].kind == .word }

func svIsCapitalised(_ s: String) -> Bool { s.unicodeScalars.first.map(isUpper) ?? false }

func svAllCaps(_ s: String) -> Bool { goUpper(s) == s && goLower(s) != s }

func svScriptUnits(_ s: String, _ language: String) -> [SvUnit] {
    let base = baseLanguage(language)
    let raws = svLex(svPre(s), hanNumbers: base == "zh")
    let english = base == "en"
    let spell = SpellOut.forLanguage(language)
    var units: [SvUnit] = []
    // Each raw's lower-case text and its parts between joiners, computed once: the phrase scans below revisit a raw
    // up to svMaxPhraseWords times.
    let lowers = raws.map { svNormApostrophes(svLower($0.text, language)) }
    let partsOf = lowers.map { fieldsFunc($0, svJoiner) }
    func word(_ k: Int) -> Bool { SaidGuard.word(raws, k) }
    func lower(_ k: Int) -> String { lowers[k] }
    // A capitalised word inside a sentence ("Main" in "on Main St."), not the first word of one.
    func innerCapital(_ k: Int) -> Bool {
        word(k) && k > 0 && svIsCapitalised(raws[k].text) && !containsAny(raws[k].gap, svSentenceEnds + ":")
    }
    func nextCapital(_ k: Int) -> Bool { word(k + 1) && svIsCapitalised(raws[k + 1].text) }
    let inExpression = svExpressions(raws, withWords: false)
    let inWordExpression = svExpressions(raws, withWords: true)

    // Spoken zeros: "oh" or "o" in a run of digits and number words with no punctuation inside ("oh oh seven",
    // "X O 4"), English only.
    var zero = [Bool](repeating: false, count: raws.count)
    if english {
        func digitish(_ k: Int) -> Bool {
            if raws[k].kind == .number { return true }
            return word(k) && svNumberWordValue(lower(k)) != nil
        }
        func oh(_ k: Int) -> Bool { word(k) && (lower(k) == "oh" || lower(k) == "o") }
        var k = 0
        while k < raws.count {
            var j = k
            var hasDigit = false
            while j < raws.count && (digitish(j) || oh(j)) && (j == k || !raws[j].punctBefore) {
                hasDigit = hasDigit || digitish(j)
                j += 1
            }
            for m in k..<j { zero[m] = hasDigit && oh(m) }
            if j == k { j += 1 }
            k = j
        }
    }
    func singleLetter(_ k: Int) -> Bool {
        if !word(k) || zero[k] { return false }
        let t = Array(raws[k].text.unicodeScalars)
        return t.count == 1 && t[0].value < 128 && isLetter(t[0])
    }

    // Each pass of the loop reads raws[spanStart..<k] into units[unitStart...]; the next pass first gives those units
    // the readings in which a dropped mark beside a word is an apostrophe.
    var spanStart = 0
    var unitStart = 0
    var k = 0
    while k < raws.count {
        defer { k += 1 }
        svKeepMarks(&units, unitStart, raws[spanStart..<k], partsOf[spanStart..<k])
        spanStart = k
        unitStart = units.count
        let r = raws[k]
        switch r.kind {
        case .number:
            if let spell, let (unit, used) = spell.groupedNumberUnit(raws, k, language) {
                units.append(unit)
                k += used - 1
                continue
            }
            units += svNumberUnits(r.text, language, spell)
            continue
        case .hanRun:
            units.append(svHanUnit(r.text))
            continue
        case .unspacedChar:
            units.append([lit(svLower(r.text, language))])
            continue
        case .symbol:
            if english && r.text == "^" && k + 1 < raws.count && raws[k + 1].gap.isEmpty
                && (raws[k + 1].text == "2" || raws[k + 1].text == "3")
            {
                let next = raws[k + 1].text
                let power = next == "2" ? "squared" : "cubed"
                let w = NumWords.cardinal(UInt64(next)!)[0]
                units.append([
                    lit("^", next), lit("^", w), lit(power), lit("to", "the", "power", "of", w), lit("to", "the", "power", "of", next),
                ])
                k += 1
                continue
            }
            // A sign before a Han numeral is the number's sign: "-五" = "-5" = "- 5".
            if r.freeDash == .sign && k + 1 < raws.count && raws[k + 1].kind == .hanRun && raws[k + 1].gap.isEmpty {
                units.append(svSignedHanUnit(raws[k + 1].text))
                k += 1
                continue
            }
            // A measurement mark after a number ("5'", "5′10″", whose ″ NFKC folds to ′′): feet or minutes, and a
            // pair, or the '"' after feet, inches or seconds. Never silent, so "5'" still refuses "5".
            if english && k > 0 && r.gap.isEmpty && (raws[k - 1].kind == .number || raws[k - 1].kind == .symbol) {
                if svMeasureMark(r.text) && raws[k - 1].kind == .number {
                    if k + 1 < raws.count && raws[k + 1].kind == .symbol && raws[k + 1].text == r.text
                        && raws[k + 1].gap.isEmpty
                    {
                        units.append([lit(r.text, r.text)] + svInchNames.map { lit($0) })
                        k += 1
                    } else {
                        units.append(svSymbolUnit(r.text, english) + svFootNames.map { lit($0) })
                    }
                    continue
                }
                if r.text == "\"" {
                    units.append([lit(r.text)] + svInchNames.map { lit($0) })
                    continue
                }
            }
            var unit = svSymbolUnit(r.text, english)
            if r.text == "-" && r.freeDash == nil && r.gap.isEmpty && k + 1 < raws.count && raws[k + 1].gap.isEmpty
                && !inExpression[k]
            {
                unit.append([])  // a range or score ("3-4", "1-0") is often read with no word for the dash
            } else if r.freeDash == .silentable && !inWordExpression[k] {
                unit.append([])  // a dash between words ("Monday - Friday", "pre- and post-war") may be unspoken
            }
            units.append(unit)
            continue
        case .word:
            break
        }
        let low = lower(k)
        if zero[k] {
            units.append([[["oh", "o", "zero", "0"]]])
            continue
        }
        // A run of single letters is a spelled code, transcribed spaced or joined ("X J" = "XJ").
        if singleLetter(k) && singleLetter(k + 1) && svSpelledGap(raws[k + 1].gap) {
            var j = k
            var letters: [String] = []
            while j < raws.count && singleLetter(j) && (j == k || svSpelledGap(raws[j].gap)) {
                letters.append(lower(j))
                j += 1
            }
            // Spaced, the reading is the letters as written. Joined, two letters with an article or pronoun among them
            // would make a word ("a T" = "at"), so a joined reading needs three letters or none of a, I.
            var unit: SvUnit = [lit(letters)]
            if letters.count >= 3 || (!letters.contains("a") && !letters.contains("i")) {
                unit.append(lit(letters.joined()))
            }
            units.append(unit)
            k = j - 1
            continue
        }
        // A domain: "www.example.com" may be read "www dot example dot com". Lowercase only, so "Dr.Smith" and a
        // sentence run together ("end.Next") are not domains. Recognised before number words, so "one.com" is a
        // domain; a label may be one letter ("x.com"), and a hyphenated label is cut as the transcript cuts it.
        var j = k + 1
        var labels = [k]
        let lowercase = low == r.text
        while lowercase && j < raws.count && word(j) && raws[j].gap == "." && lower(j) == raws[j].text {
            labels.append(j)
            j += 1
        }
        if labels.count >= 2 {  // "one.Next" has no second lowercase label: a number word and a word
            // One unit a label and one a dot, so each choice is independent and the readings stay linear in the
            // labels: a hyphenated label split or joined on its own ("e-mail.long-term.com" = "email dot long term
            // dot com"), and "dot" said or not at each dot. A dot written by the transcript is unspoken, so
            // "www.example dot com" already reads as "www example dot com": a per-dot choice accepts no reading the
            // written dots did not.
            for (i, l) in labels.enumerated() {
                if i > 0 { units.append([lit("dot"), []]) }
                var unit: SvUnit = [lit(partsOf[l])]
                if partsOf[l].count > 1 {  // "e-mail.com" = "email dot com", as a hyphenated word may be joined
                    unit.append(lit(partsOf[l].joined()))
                }
                units.append(unit)
            }
            k = j - 1
            continue
        }
        // A number in words ("a hundred and sixty", "forty-two", "minus seven", "three point five percent",
        // "twenty-first"): the longest prefix of at most svMaxPhraseWords words that reads as one number.
        if english, let first = partsOf[k].first, svPhraseWord(first) {
            var words: [String] = []
            var ends: [Int] = []  // ends[l-1]: the words in the first l raws
            var j = k
            while j < raws.count && j < k + svMaxPhraseWords && word(j) && (j == k || !raws[j].punctBefore) {
                let parts = partsOf[j]
                // The bound counts words, hyphenated ones too.
                if parts.isEmpty || !parts.allSatisfy(svPhraseWord) || words.count + parts.count > svMaxPhraseWords { break }
                words += parts
                ends.append(words.count)
                j += 1
            }
            var found = false
            var l = ends.count
            while l > 0 && !found {
                if let digits = svParseEnglishPhrase(words[..<ends[l - 1]]) {
                    // The words keep their ordinary readings ("TEN" = "T E N", "forty-two" = "fortytwo") beside
                    // the number's, up to svMaxProduct of them; a longer phrase is read as written and as a number.
                    let plain = (k..<k + l).map { svWordUnit(raws, $0, lower($0), partsOf[$0], english, innerCapital, nextCapital) }
                    var unit = svProduct(plain) ?? [lit(Array(words[..<ends[l - 1]]))]
                    for reading in svNumberUnits(digits, language, nil)[0] where !unit.contains(reading) {
                        unit.append(reading)
                    }
                    units.append(unit)
                    k += l - 1
                    found = true
                }
                l -= 1
            }
            if found { continue }
        }
        // A number in the locale's words ("vier", "vingt et un"): a spelling of the value it is parsed as.
        if let spell,
            let (found, used) = spell.wordNumberUnits(
                raws, partsOf, k, ordinary: { svWordUnit(raws, $0, lower($0), partsOf[$0], english, innerCapital, nextCapital) })
        {
            units += found
            k += used - 1
            continue
        }
        // "for example" and "et cetera" may be transcribed abbreviated.
        if english && word(k + 1) && !raws[k + 1].punctBefore {
            if let abbr = svAbbreviated[low + " " + lower(k + 1)] {
                units.append([lit(low, lower(k + 1)), lit(abbr)])
                k += 1
                continue
            }
        }
        units.append(svWordUnit(raws, k, low, partsOf[k], english, innerCapital, nextCapital))
    }
    svKeepMarks(&units, unitStart, raws[spanStart...], partsOf[spanStart...])
    return units
}

/// Gives the units from `from` on, read from the raws of `span`, the readings in which a mark ``svUnquote(_:)``
/// dropped beside one of their words is an apostrophe ("‘the students’ books’" may say "the students’ books"). A
/// reading that spells words as written (their parts, in order, from the first word not yet placed) takes each word's
/// marks on its own parts; the unit's other readings of those words ("email" for "e-mail", "100" for "a hundred") take
/// the first word's leading mark and the last word's trailing one. A unit with no reading of the words as written
/// ("dot" in a domain) is left as it is. Each reading gains at most three spellings per position, so the readings stay
/// linear in the marks.
func svKeepMarks(_ units: inout [SvUnit], _ from: Int, _ span: ArraySlice<SvRaw>, _ parts: ArraySlice<[String]>) {
    var words: [(text: String, lead: Bool, trail: Bool)] = []
    var marked = false
    for (r, ps) in zip(span, parts) where r.kind == .word {
        for (i, p) in ps.enumerated() {
            words.append((p, i == 0 && r.optLead, i == ps.count - 1 && r.optTrail))
        }
        marked = marked || r.optLead || r.optTrail
    }
    if !marked { return }
    func spells(_ seq: SvSeq, _ at: Int) -> Bool {
        if seq.isEmpty || at + seq.count > words.count { return false }
        for (p, alt) in seq.enumerated() where !alt.contains(words[at + p].text) { return false }
        return true
    }
    var at = 0
    for u in units.indices.dropFirst(from) {
        let unit = units[u]
        guard let literal = unit.firstIndex(where: { spells($0, at) }) else { continue }
        let n = unit[literal].count
        let first = words[at]
        let last = words[at + n - 1]
        var out: SvUnit = []
        for (i, seq) in unit.enumerated() {
            var ns = seq
            if i == literal {
                for p in ns.indices { ns[p] = svWithMarks(ns[p], words[at + p].lead, words[at + p].trail) }
            } else if ns.count == 1 {
                ns[0] = svWithMarks(ns[0], first.lead, last.trail)
            } else if ns.count > 1 {
                ns[0] = svWithMarks(ns[0], first.lead, false)
                ns[ns.count - 1] = svWithMarks(ns[ns.count - 1], false, last.trail)
            }
            out.append(ns)
        }
        units[u] = out
        at += n
    }
}

/// Each token of `alt` in all its ``svMarkReadings(_:lead:trail:)``.
func svWithMarks(_ alt: SvAlt, _ lead: Bool, _ trail: Bool) -> SvAlt {
    if !lead && !trail { return alt }
    return alt.flatMap { svMarkReadings($0, lead: lead, trail: trail) }
}

func svWordUnit(
    _ raws: [SvRaw], _ k: Int, _ low: String, _ parts: [String], _ english: Bool, _ innerCapital: (Int) -> Bool,
    _ nextCapital: (Int) -> Bool
) -> SvUnit {
    let r = raws[k]
    var unit: SvUnit = [lit(parts)]
    if parts.count > 1 { unit.append(lit(parts.joined())) }  // "e-mail" = "email"
    if !english { return svWithCodeReadings(unit, raws, k, low) }
    let bare = r.text  // the lexer keeps no dot in a word ("e.g." is "e.g")
    // A title only with its capital and before a name: "ms" is milliseconds, "5 Ms." is no one. "Dr" and "St" after a
    // capitalised word inside a sentence are a road ("Take Oak Dr.", "Main St. Next").
    let road =
        !containsAny(r.gap, svSentenceEnds)
        && (innerCapital(k - 1) || (word(raws, k - 1) && svIsCapitalised(raws[k - 1].text) && !nextCapital(k)))
    if let exp = svTitles[bare], nextCapital(k), !(road && bare == "Dr") { unit.append(lit(exp)) }
    if let exp = svSuffixTitles[bare], word(raws, k - 1), svIsCapitalised(raws[k - 1].text) {
        unit.append(lit(exp))  // after a name: "John Smith Jr."
    }
    if bare == "Dr" && road {
        unit.append(lit("drive"))
    } else if bare == "St" && road {
        unit.append(lit("street"))
    } else if bare == "St" && nextCapital(k) && !road {
        unit.append(lit("saint"))
    }
    if let abbr = svTitleAbbrev[low], nextCapital(k) {
        unit.append(lit(abbr))  // "Doctor Smith" may be transcribed "Dr. Smith"
    }
    // "vs", "etc", "e.g", "i.e" expand only in lower case (a capital first letter is a sentence start): "VS Code" is a
    // code and "E.G. White" initials.
    let tail = svString(bare.unicodeScalars.dropFirst())
    if let exp = svCaseFree[goLower(bare)], goLower(tail) == tail {
        unit.append(lit(exp))
        if bare.contains(".") {
            unit.append(lit(split(goLower(bare), ".")))
            unit.append(lit(goLower(bare).replacingOccurrences(of: ".", with: "")))
        }
    }
    return svWithCodeReadings(unit, raws, k, low)
}

/// Common short English words written in capitals for emphasis ("Turn ON", "He said NO"); they are never spelled.
/// Short codes that are also words (US, IT) are ambiguous; they stay codes, and OK is spelled.
private let svCapsWords: Set<String> = Set(
    """
    AM AN AS AT BE BY DO GO HE HI IF IN IS ME MY NO OF OH ON OR SO TO UP WE YO
    ALL AND ANY ARE ASK BAD BIG BOY BUT CAN DAY DID END FAR FEW FOR GET GOT HAD HAS HER HEY HIM HIS HOT HOW ITS LET
    LOT MAN MAY NEW NOT NOW OFF OLD ONE OUR OUT OWN PUT RUN SAY SEE SET SHE SIT TOO TOP TRY TWO USE WAS WAY WHO WHY
    WIN YES YET YOU
    """.split(whereSeparator: { $0 == " " || $0 == "\n" }).map(String.init))

/// A letter-by-letter reading for a code: an all-capital word of two or three letters, or of up to six with no vowel
/// ("API", "HTML"), unless a neighbour is capitalised too (a shouted "STOP NOW"); and two or three letters written
/// against a number ("mp3", "Nf3"), or a capital code of up to six against one ("JIRA-10442").
func svWithCodeReadings(_ unit: SvUnit, _ raws: [SvRaw], _ k: Int, _ low: String) -> SvUnit {
    let text = raws[k].text
    let n = text.unicodeScalars.count
    // A capital neighbour that is a word, not a code, makes this shouting ("STOP NOW"); two codes stay codes ("API URL").
    func shout(_ j: Int) -> Bool {
        if j < 0 || j >= raws.count || raws[j].kind != .word || !svAllCaps(raws[j].text) { return false }
        return (raws[j].text.unicodeScalars.count >= 4 && containsAny(raws[j].text, "AEIOUY"))
            || svCapsWords.contains(raws[j].text)
    }
    func attached(_ gap: String) -> Bool { gap.isEmpty || gap == "-" }
    let glued =
        (k + 1 < raws.count && raws[k + 1].kind == .number && attached(raws[k + 1].gap))
        || (k > 0 && raws[k - 1].kind == .number && attached(raws[k].gap))
    let plain = !containsAny(text, "'-‐‑")
    let code =
        svAllCaps(text) && !svCapsWords.contains(text) && !shout(k - 1) && !shout(k + 1)
        && ((n >= 2 && n <= 3) || (n <= 6 && (glued || !containsAny(goUpper(text), "AEIOUY"))))
    if plain && (code || (glued && n >= 2 && n <= 3)) { return unit + [lit(svSplitRunes(low))] }
    return unit
}

/// Every concatenation of one reading per unit, or nil beyond svMaxProduct readings.
func svProduct(_ units: [SvUnit]) -> SvUnit? {
    var out: SvUnit = [[]]
    for u in units {
        var next: SvUnit = []
        for a in out {
            for b in u {
                next.append(a + b)
                if next.count > svMaxProduct { return nil }
            }
        }
        out = next
    }
    return out
}

/// The most readings a phrase's words keep beside the number they spell (``svProduct(_:)``).
let svMaxProduct = 64

/// The operators that make a run holding words an expression (``svExpressions(_:withWords:)``).
private let svMathOperators: Set<String> = ["=", "+", "−", "×", "÷", "<", ">", "≠", "*", "^"]

/// Marks each raw that sits in an expression: a run of numbers and symbols joined by nothing but space that holds an
/// operator other than "-" or a number's "'" or '"' ("5-2=3"). A dash there is a spoken minus. Punctuation ends a run, so
/// in "1-0. #1" the score's dash is not in one. `withWords` lets words into the run but counts only the arithmetic
/// operators (`svMathOperators`): in "x - y = z" the free dash is a minus, in "Tom & Jerry - live" it may be
/// silent. One pass over the raws, so a long chain costs its length.
func svExpressions(_ raws: [SvRaw], withWords: Bool) -> [Bool] {
    var inside = [Bool](repeating: false, count: raws.count)
    func operand(_ j: Int) -> Bool {
        switch raws[j].kind {
        case .number, .hanRun, .symbol: return true
        case .word: return withWords
        // An unspaced character is a word of its own, and a Han numeral outside zh still a number ("五-二=三" in ja).
        case .unspacedChar: return withWords || raws[j].text.unicodeScalars.first.map(isHanNumeral) == true
        }
    }
    func op(_ j: Int) -> Bool {
        guard raws[j].kind == .symbol else { return false }
        return withWords ? svMathOperators.contains(raws[j].text) : !["-", "'", "\""].contains(raws[j].text)
    }
    var i = 0
    while i < raws.count {
        guard operand(i) else {
            i += 1
            continue
        }
        var j = i
        var holds = false
        while j < raws.count && operand(j) && (j == i || !raws[j].punctBefore) {
            holds = holds || op(j)
            j += 1
        }
        for m in i..<j { inside[m] = holds }
        i = j
    }
    return inside
}

/// The characters that end a sentence. NFKC folds the full-width ！ and ？ but not the ideographic full stop.
let svSentenceEnds = ".!?。"

/// The English readings of a measurement mark after a number (``svMeasureMark(_:)``), and of two of them or the '"'
/// after feet.
private let svFootNames = ["feet", "foot", "minutes", "minute"]
private let svInchNames = ["inches", "inch", "seconds", "second"]

/// A Han numeral after its sign: each reading of the numeral after the sign as a token of its own ("-" or "−"), and
/// a reading in digits also with the sign joined to it, as the transcript lexes "-5".
func svSignedHanUnit(_ run: String) -> SvUnit {
    var unit: SvUnit = []
    for seq in svHanUnit(run) {
        unit.append([["-", "−"]] + seq)
        guard let first = seq.first else { continue }
        let signed = first.filter { $0.unicodeScalars.first.map(svDigit) ?? false }.map { "-" + $0 }
        if !signed.isEmpty { unit.append([signed] + seq.dropFirst()) }
    }
    return unit
}

/// The English names of spoken symbols. A symbol without a name matches only itself.
private let svSymbolNames: [String: [String]] = [
    "%": ["percent"], "+": ["plus"], "=": ["equals", "equal", "is"], "&": ["and"], "@": ["at"],
    "°": ["degrees", "degree"], "-": ["minus", "to"], "−": ["minus"], "×": ["times", "multiplied by"], "÷": ["divided by"],
    "<": ["less than"], ">": ["greater than"], "≠": ["not equal to", "does not equal"], "#": ["number", "hash", "sharp"],
    "*": ["star", "asterisk", "times"], "′": ["prime"], "^": ["to the power of"],
]

func svSymbolUnit(_ sym: String, _ english: Bool) -> SvUnit {
    var unit: SvUnit = [lit(sym)]
    if sym == "-" { unit.append(lit("−")) }  // the two minuses are written either way
    if sym == "−" { unit.append(lit("-")) }
    if english {
        for n in svSymbolNames[sym] ?? [] { unit.append(lit(n.split(separator: " ").map(String.init))) }
    }
    return unit
}
