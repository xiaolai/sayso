import Foundation

// --- lexer ---

enum SvKind { case word, number, unspacedChar, hanRun, symbol }

/// The role of a free dash, or of a '−' that is no minus between numbers (``svDashRole(_:_:)``): a sign ("-x", "-五"),
/// a dash before a number that is no sign ("A-五"), both never silent, or a dash the script side may read with no word
/// outside an expression ("Monday - Friday").
enum SvFreeDash { case sign, spoken, silentable }

struct SvRaw {
    let text: String
    let kind: SvKind
    /// The text between the previous raw and this one.
    let gap: String
    /// For a word whose first or last letter touches marks ``svUnquote(_:)`` dropped as possible quotation marks, how
    /// many: they may as well be apostrophes, so the word may also be read with them ("‘the students’ books’",
    /// "''yes''").
    var optLead = 0
    var optTrail = 0
    /// Set on a symbol that is a free dash, nil on any other raw.
    var freeDash: SvFreeDash?

    /// Something other than space separates this raw from the one before ("Oh, one").
    var punctBefore: Bool { !trimSpace(gap).isEmpty }
}

/// The closed set of punctuation no one reads aloud; both sides drop it. Every other character that is not a letter,
/// digit, mark or space is a spoken symbol and a token on both sides, so a transcript cannot gain or lose one.
private let svUnspokenPunct = Set(".,;:!?\"'`()[]{}…–—―/\\|_~·•¡¿、。，！？；：「」『』（）《》〈〉【】‹›«»“”„‘’‚".unicodeScalars)
private let svSpokenSo = Set("°©®™№".unicodeScalars)

func svSpokenSymbol(_ r: Unicode.Scalar) -> Bool {
    if isLetter(r) || isNd(r) || isMark(r) || isSpace(r) || isControl(r) || isUnspaced(r) { return false }
    if svUnspokenPunct.contains(r) || svJoiner(r) { return false }
    if r.properties.generalCategory == .otherSymbol && !svSpokenSo.contains(r) {
        return false  // emoji and pictographs are not read aloud
    }
    return true
}

/// A '-' with a number on each side, spaced or not ("5 - 2", "3-4", "5 -2", "5% - 2%", "1st - 3rd", "五 - 二"), is
/// spoken (minus, to). A number on the left may end in a percent sign or an ordinal suffix, as the lexer reads it; the
/// one on the right may carry a sign ("5 - -2", "5 - +2") or start with a decimal point or comma ("5 - .2", "5 - ,2").
func svBetweenNumbers(_ rs: [Unicode.Scalar], _ i: Int) -> Bool {
    var p = i - 1
    var n = i + 1
    while p >= 0 && isSpace(rs[p]) { p -= 1 }
    while n < rs.count && isSpace(rs[n]) { n += 1 }
    if p >= 1 && rs[p] == "%" {
        p -= 1
        while p >= 0 && isSpace(rs[p]) { p -= 1 }  // "5 % - 2 %"
    } else if p >= 2 && svOrdinalSuffixAt(rs, p - 1) {
        p -= 2
    }
    if n < rs.count && svMinus(rs[n]) {
        if let o = svSignOperand(rs, n) { n = o }  // a signed operand, its sign spaced or not ("5 - -2", "5 - - 2")
    } else if n < rs.count && rs[n] == "+" && svOperandAt(rs, n + 1) {
        n += 1  // "5 - +2"
    }
    return p >= 0 && (svDigit(rs[p]) || isHanNumeral(rs[p])) && svOperandAt(rs, n)
}

/// A number starts at `rs[i]`: a digit, a Han numeral, or a decimal point or comma before a digit.
func svOperandAt(_ rs: [Unicode.Scalar], _ i: Int) -> Bool {
    guard i < rs.count else { return false }
    return svDigit(rs[i]) || isHanNumeral(rs[i])
        || ((rs[i] == "." || rs[i] == ",") && i + 1 < rs.count && svDigit(rs[i + 1]))
}

/// The hyphen-minus or the minus sign.
func svMinus(_ r: Unicode.Scalar) -> Bool { r == "-" || r == "−" }

/// `rs[i..<i+2]` is "st", "nd", "rd" or "th" in any case.
func svOrdinalSuffixAt(_ rs: [Unicode.Scalar], _ i: Int) -> Bool {
    guard i >= 0, i + 2 <= rs.count else { return false }
    switch String(String.UnicodeScalarView(rs[i..<i + 2])).lowercased() {
    case "st", "nd", "rd", "th": return true
    default: return false
    }
}

/// A '-' or '−' before a digit, or a decimal point or comma before one ("-.5", "-,5"), with or without spaces between
/// ("- 5"), is a sign at the start, after a space, punctuation or an unspaced character, or as an exponent sign
/// ("1e-3"); after a letter or digit it is none: a joiner when glued ("JIRA-10442"), and after a number, spaced or
/// not, a minus ("3-4", "5 -2", which ``svBetweenNumbers(_:_:)`` decides first). The operand decides, so the spaces
/// before it never do. Returns the index of the operand, or nil when rs[i] is no such sign.
func svSignOperand(_ rs: [Unicode.Scalar], _ i: Int) -> Int? {
    guard svMinus(rs[i]) else { return nil }
    let o = svPastSpaces(rs, i + 1)
    guard o < rs.count, svDigit(rs[o]) || ((rs[o] == "." || rs[o] == ",") && o + 1 < rs.count && svDigit(rs[o + 1]))
    else { return nil }
    if i == 0 { return o }
    let p = rs[i - 1]
    if o == i + 1 && (p == "e" || p == "E") && i >= 2 && svDigit(rs[i - 2]) { return o }
    return !svLetter(p) && !svDigit(p) && !isMark(p) ? o : nil
}

func svSignAt(_ rs: [Unicode.Scalar], _ i: Int) -> Bool { svSignOperand(rs, i) != nil }

/// The first index from i on that holds no space, rs.count at the end.
func svPastSpaces(_ rs: [Unicode.Scalar], _ i: Int) -> Int {
    var j = i
    while j < rs.count && isSpace(rs[j]) { j += 1 }
    return j
}

/// For each index, the first index from it on that holds no apostrophe, so a run of marks of any length is read as
/// one ("''yes''", "五’’’"); rs.count at the end. One reverse pass.
func svMarkRunEnds(_ rs: [Unicode.Scalar]) -> [Int] {
    var end = [Int](repeating: rs.count, count: rs.count + 1)
    for i in rs.indices.reversed() { end[i] = isApostrophe(rs[i]) ? end[i + 1] : i }
    return end
}

func isHanNumeral(_ r: Unicode.Scalar) -> Bool {
    svHanDigit[r] != nil || svHanBigUnit[r] != nil || svHanSmallUnit[r] != nil
}

private let svNumberSeparators = Set(".,/:".unicodeScalars)

func svLex(_ s: String, hanNumbers: Bool) -> [SvRaw] {
    let (rs, dropped) = svUnquote(Array(s.unicodeScalars))
    let markEnd = svMarkRunEnds(rs)
    // Whether the marks from rs[i] on, however many, are followed by a letter ("’tis", "''yes").
    func marksBeforeLetter(_ i: Int) -> Bool { markEnd[i] < rs.count && svLetter(rs[markEnd[i]]) }
    // The dropped marks touching rs[i] on its left, and from rs[j] on: each is walked from at most the two words
    // beside its run, so the work stays linear.
    func droppedBefore(_ i: Int) -> Int {
        var n = 0
        while i - n > 0 && dropped[i - n - 1] { n += 1 }
        return n
    }
    func droppedFrom(_ j: Int) -> Int {
        var n = 0
        while j + n < rs.count && dropped[j + n] { n += 1 }
        return n
    }
    var out: [SvRaw] = []
    var prevEnd = 0
    // The end of the last number or unspaced character, or of the run of kept marks touching one, carried forward so
    // a run of any length costs its length.
    var tokenEnd = -1
    func emit(_ kind: SvKind, _ i: Int, _ j: Int, freeDash: SvFreeDash? = nil, body: Int? = nil) {
        // One sign, however written and however far from its digits: "−5" and "- 5" read as "-5"; one apostrophe,
        // however written, as a symbol.
        var text = svString(rs[i..<j])
        if kind == .number && svMinus(rs[i]) { text = "-" + svString(rs[(body ?? i + 1)..<j]) }
        if kind == .symbol && isApostrophe(rs[i]) { text = "'" }
        var raw = SvRaw(text: text, kind: kind, gap: svString(rs[prevEnd..<i]), freeDash: freeDash)
        if kind == .word {
            raw.optLead = droppedBefore(i)
            raw.optTrail = droppedFrom(j)
        }
        out.append(raw)
        switch kind {
        case .number, .hanRun, .unspacedChar: tokenEnd = j
        case .symbol where text == "'" && i == tokenEnd: tokenEnd = j
        default: tokenEnd = -1
        }
        prevEnd = j
    }
    var i = 0
    while i < rs.count {
        let r = rs[i]
        // Only a minus looks across the spaces around it: asked at every space, a run of them would cost its square.
        let between = svMinus(r) && svBetweenNumbers(rs, i)
        let sign = between ? nil : svSignOperand(rs, i)
        if svDigit(r) || sign != nil
            || (r == "." && i + 1 < rs.count && svDigit(rs[i + 1]) && (i == 0 || !svDigit(rs[i - 1])))
            || (r == "," && i + 1 < rs.count && svDigit(rs[i + 1]) && (i == 0 || isSpace(rs[i - 1])))
        {
            // A leading "," is a number only after a space (",5" is a French decimal); "Yes,3" is a word and a number.
            var j = sign ?? i + 1
            while j < rs.count && svDigit(rs[j]) { j += 1 }
            while j + 1 < rs.count && svNumberSeparators.contains(rs[j]) && svDigit(rs[j + 1]) {
                j += 2
                while j < rs.count && svDigit(rs[j]) { j += 1 }
            }
            if svDigit(rs[j - 1]) && svOrdinalSuffixAt(rs, j) && (j + 2 >= rs.count || !svLetter(rs[j + 2])) {
                j += 2
            }
            if j < rs.count && rs[j] == "%" { j += 1 }
            emit(.number, i, j, body: sign)
            i = j
        } else if between {
            emit(.symbol, i, i + 1)
            i += 1
        } else if r == "−" {
            // A minus sign is spoken wherever it stands; before a Han numeral or a word it may be a sign.
            emit(.symbol, i, i + 1, freeDash: svDashRole(rs, i) == .sign ? .sign : .spoken)
            i += 1
        } else if svSpokenSymbol(r) {
            emit(.symbol, i, i + 1)
            i += 1
        } else if svFreeDash(rs, i) {
            emit(.symbol, i, i + 1, freeDash: svDashRole(rs, i))
            i += 1
        } else if isApostrophe(r) && !marksBeforeLetter(i)
            && (tokenEnd == i || (markEnd[i] < rs.count && isUnspaced(rs[markEnd[i]])))
        {
            // Marks right after a number or an unspaced character that svUnquote(_:) kept, however many, are no
            // quotation marks: feet or minutes ("5'", "5’10", "21st’"), with a second one inches or seconds ("5''"),
            // or marks of unspaced text on either side ("五’’’", "好’", "’’好"), kept as a word keeps its own ("’tis").
            // Each is a spoken symbol on both sides, so a transcript cannot drop one.
            emit(.symbol, i, i + 1)
            i += 1
        } else if r == "\"" && !dropped[i] && prevEnd == i && svFeetAndNumber(out) {
            // The inches after feet ("5'10\""): a symbol on both sides. Any other '"' is a quotation mark, unspoken.
            emit(.symbol, i, i + 1)
            i += 1
        } else if hanNumbers && isHanNumeral(r) && !(i + 1 < rs.count && isMark(rs[i + 1])) {
            // A numeral carrying a mark is a character like any other ("一́"), as the transcript side reads it.
            var j = i
            while j < rs.count && isHanNumeral(rs[j]) && !(j + 1 < rs.count && isMark(rs[j + 1])) { j += 1 }
            emit(.hanRun, i, j)
            i = j
        } else if isUnspaced(r) {
            var j = i + 1
            while j < rs.count && isMark(rs[j]) { j += 1 }
            emit(.unspacedChar, i, j)
            i = j
        } else if svLetter(r) || (isApostrophe(r) && marksBeforeLetter(i)) {
            // "e.g." and "i.e." are one word, also at a sentence start ("E.g."). With a capital second letter they are
            // initials ("E.G. White"), lexed letter by letter like any other; with a mark on it ("e.g̣") another word.
            // Scalars, not graphemes: a symbol after the g, as an emoji modifier, is a token of its own, as in Go.
            let m = goLower(svString(rs[i..<min(i + 4, rs.count)])).unicodeScalars
            // Not inside a domain ("www.e.g.com"), where its letters are labels.
            if (m.starts(with: "e.g".unicodeScalars) || m.starts(with: "i.e".unicodeScalars)) && !isUpper(rs[i + 2])
                && (i == 0 || (!svLetter(rs[i - 1]) && rs[i - 1] != "."))
                && !(i + 4 < rs.count && rs[i + 3] == "." && svLetter(rs[i + 4]))
                && (i + 3 >= rs.count || (!svLetter(rs[i + 3]) && !isMark(rs[i + 3])))
            {
                emit(.word, i, i + 3)
                i += 3
                if i < rs.count && rs[i] == "." {
                    i += 1
                    out[out.count - 1].optTrail = droppedFrom(i)  // the dot is the abbreviation's: "‘e.g.’"
                }
                continue
            }
            var j = i
            while j < rs.count {
                let c = rs[j]
                if svLetter(c) || isMark(c) {
                    j += 1
                } else if isApostrophe(c) && (j > i || marksBeforeLetter(j)) {
                    j += 1
                } else if svJoiner(c) && j > i && j + 1 < rs.count && svLetter(rs[j + 1])
                    && (svLetter(rs[j - 1]) || isMark(rs[j - 1]))
                {
                    j += 1
                } else {
                    break
                }
            }
            emit(.word, i, j)
            i = j
        } else {
            i += 1
        }
    }
    return out
}

/// Whether the raws so far are a number, a feet mark and a number, all touching ("5'10", "5′10").
func svFeetAndNumber(_ out: [SvRaw]) -> Bool {
    let n = out.count
    return n >= 3 && out[n - 1].kind == .number && out[n - 1].gap.isEmpty && out[n - 2].kind == .symbol
        && svMeasureMark(out[n - 2].text) && out[n - 2].gap.isEmpty && out[n - 3].kind == .number
}

/// A mark that measures after a number: the apostrophe the lexer keeps there, or the prime.
func svMeasureMark(_ s: String) -> Bool { s == "'" || s == "′" }

/// A '-' the lexer read as no sign, no minus between numbers and no joiner inside a word, unless it is glued between
/// a letter or digit and a letter or digit ("JIRA-10442"), which stays silent: a dash between words ("Monday -
/// Friday", "x - y = z"), a suspended hyphen ("pre- and post-war") or a sign before a word or Han numeral ("-x",
/// "-五"). It is a spoken symbol on both sides, so a transcript cannot drop the one an expression or a sign needs.
func svFreeDash(_ rs: [Unicode.Scalar], _ i: Int) -> Bool {
    guard rs[i] == "-" else { return false }
    let glued = i > 0 && i + 1 < rs.count && (svLetter(rs[i - 1]) || svDigit(rs[i - 1]) || isMark(rs[i - 1]))
        && (svLetter(rs[i + 1]) || svDigit(rs[i + 1]))
    return !glued
}

/// The role of a free dash (spec/said-guard.md, the dash table). Before a number, a Han numeral as a digit, past any
/// spaces, the operand decides: a sign unless a letter, digit or mark comes before it, as ``svSignOperand(_:_:)`` has
/// it ("-五", "- 五", "气温-五度"), and otherwise a spoken dash ("A-五", "x- 5"); never silent either way. Before a
/// word or any other unspaced character it is a sign only touching it at a word start, after a space, punctuation or
/// the start ("-x"): a spaced dash before a word separates ("Monday - Friday", "- item"), and a dash after an unspaced
/// character joins two words ("北京-上海"), as a hyphen joins "x-y". Any other free dash may be silent.
func svDashRole(_ rs: [Unicode.Scalar], _ i: Int) -> SvFreeDash {
    let n = svPastSpaces(rs, i + 1)
    guard n < rs.count else { return .silentable }
    let afterWord = i > 0 && (svLetter(rs[i - 1]) || svDigit(rs[i - 1]) || isMark(rs[i - 1]))
    if svOperandAt(rs, n) { return afterWord ? .spoken : .sign }
    if n == i + 1 && !afterWord && !(i > 0 && isUnspaced(rs[i - 1])) && (svLetter(rs[n]) || isUnspaced(rs[n])) {
        return .sign
    }
    return .silentable
}

private let svQuoteOpeners = Set("([{\"“«".unicodeScalars)
private let svSentenceEndScalars = Set(".!?。".unicodeScalars)
private let svDotAbbreviations: Set<String> = ["mr", "mrs", "ms", "dr", "st", "jr", "sr", "vs"]

/// Whether rs[i] ends a sentence: "!", "?" and "。" always, and "." unless a letter or digit follows it ("e.g",
/// "3.5", "x.com") or it closes an abbreviation that is mostly followed by more of the sentence (Mr, Mrs, Ms, Dr, St,
/// Jr, Sr, vs, e.g., i.e.; "etc." mostly ends one). It looks at most four letters back, so the work is linear.
func svSentenceEndAt(_ rs: [Unicode.Scalar], _ i: Int) -> Bool {
    let r = rs[i]
    guard svSentenceEndScalars.contains(r) else { return false }
    if r != "." { return true }
    if i + 1 < rs.count && (svLetter(rs[i + 1]) || svDigit(rs[i + 1])) { return false }
    var b = i
    while b > 0 && i - b < 4 && svLetter(rs[b - 1]) { b -= 1 }
    if b == i || (b > 0 && (svLetter(rs[b - 1]) || svDigit(rs[b - 1]))) { return true }  // "1st.", a longer word
    let w = svASCIILower(rs[b..<i])
    if svDotAbbreviations.contains(w) && (b == 0 || rs[b - 1] != ".") { return false }
    // The dot after "e.g" and "i.e", not inside a domain ("x.e.g.").
    if w.count == 1 && b >= 2 && rs[b - 1] == "." && svLetter(rs[b - 2]) {
        let pair = svASCIILower(rs[b - 2..<b - 1]) + "." + w
        return !((pair == "e.g" || pair == "i.e") && (b == 2 || (!svLetter(rs[b - 3]) && rs[b - 3] != ".")))
    }
    return true
}

/// The ASCII letters of `rs` in lower case; any other scalar is kept as it is.
private func svASCIILower(_ rs: ArraySlice<Unicode.Scalar>) -> String {
    var out = ""
    for r in rs { out.unicodeScalars.append(r.value >= 0x41 && r.value <= 0x5A ? Unicode.Scalar(r.value + 0x20)! : r) }
    return out
}

/// The English words written with a leading apostrophe for dropped letters ("’tis", "’em", "’n’"). A mark before one
/// is an apostrophe, never an opening quotation mark: a closed list, as the guard's other word lists.
let svElisions: Set<String> = [
    "tis", "twas", "twere", "twill", "twould", "em", "n", "cause", "cos", "til", "bout", "cept", "neath", "gainst",
    "tween", "twixt", "scuse", "nuff",
]

/// Whether the letters starting at rs[i], in any case, are a whole word of ``svElisions``: not a word's prefix
/// before a mark, or before a joiner or an apostrophe and a letter ("n-gram", "n'est").
func svElisionAt(_ rs: [Unicode.Scalar], _ start: Int) -> Bool {
    var b = ""
    var i = start
    while i < rs.count && svLetter(rs[i]) {
        var v = rs[i].value
        if v >= 0x41 && v <= 0x5A { v += 0x20 }
        if v < 0x61 || v > 0x7A || b.utf8.count == 6 { return false }
        b.unicodeScalars.append(Unicode.Scalar(v)!)
        i += 1
    }
    if i < rs.count
        && (isMark(rs[i]) || ((svJoiner(rs[i]) || isApostrophe(rs[i])) && i + 1 < rs.count && svLetter(rs[i + 1])))
    {
        return false
    }
    return svElisions.contains(b)
}

/// Finds the marks that may be quotation marks around words ("Say ‘hello’.", "'yes'", "''yes''") and replaces each
/// with an unspoken '"', reporting which. A run of marks of any length is read as one: it opens or closes as a whole. It does not decide which mark closes which: in "‘the students’ books’" the middle
/// mark may be a possessive or a closing quotation mark. A dropped mark leaves its word flagged (``SvRaw/optLead``,
/// ``SvRaw/optTrail``), and each side then reads the word with and without the mark, so the transcript selects. An
/// opener stands at a word start after a space, an opening bracket or the start, and never before a leading elision
/// (``svElisions``: "’Tis the students’ books" holds two apostrophes); a closer at a word end, after any punctuation
/// that ends the quoted words ("‘hello!’", "‘yes,’"), before a space, punctuation or the end. Within one sentence,
/// which ends at its final punctuation (``svSentenceEndAt(_:_:)``) or, where closers follow that, right after them
/// ("‘Wait.’" is one quotation, and "‘Wait!’ The students’ books." two sentences), an opener with a closer after
/// it, and a closer with an opener before it, may be quotation marks; every other mark is an apostrophe ("’tis", "the
/// students’ books", "rock ’n’ roll"). The candidates of a sentence are kept in a list, and the next scalar that is
/// no inner punctuation is found in one reverse pass, so the work is linear.
func svUnquote(_ input: [Unicode.Scalar]) -> ([Unicode.Scalar], [Bool]) {
    var rs = input
    var dropped = [Bool](repeating: false, count: rs.count)
    // The runs of marks that may open or close, as ranges; a run between two unspaced characters may be both.
    var openers: [Range<Int>] = []
    var closers: [Range<Int>] = []
    func flush() {
        if let first = openers.first?.lowerBound, let last = closers.last?.lowerBound {
            for o in openers where o.lowerBound < last { for m in o { dropped[m] = true } }
            for c in closers where c.lowerBound > first { for m in c { dropped[m] = true } }
        }
        openers.removeAll(keepingCapacity: true)
        closers.removeAll(keepingCapacity: true)
    }
    let markEnd = svMarkRunEnds(rs)
    // next[i]: the first index from i on that holds no ``svInnerPunct(_:)``, rs.count at the end.
    var next = [Int](repeating: rs.count, count: rs.count + 1)
    for i in rs.indices.reversed() { next[i] = svInnerPunct(rs[i]) ? next[i + 1] : i }
    func markFrom(_ i: Int) -> Int? { next[i] < rs.count && isApostrophe(rs[next[i]]) ? next[i] : nil }
    // The mark after a sentence's final punctuation: the sentence ends after it, or before it when it closes nothing.
    var pending: Int?
    var i = 0
    while i < rs.count {
        if !isApostrophe(rs[i]) {
            if svSentenceEndAt(rs, i) {
                if let m = markFrom(i + 1) { pending = m } else { flush() }
            }
            i += 1
            continue
        }
        let e = markEnd[i]  // the run rs[i..<e], tested by what stands before and after it
        var k = i - 1
        while k >= 0 && svInnerPunct(rs[k]) { k -= 1 }
        // An unspaced character is a word of its own, so it is a word boundary on either side, as a space is.
        let closer = k >= 0 && (svLetter(rs[k]) || isMark(rs[k]) || svDigit(rs[k]) || isUnspaced(rs[k]))
            && (e == rs.count || isSpace(rs[e]) || isUnspaced(rs[e]) || svUnspokenPunct.contains(rs[e]))
        let opener = (i == 0 || isSpace(rs[i - 1]) || isUnspaced(rs[i - 1]) || svQuoteOpeners.contains(rs[i - 1]))
            && e < rs.count && (svLetter(rs[e]) || svDigit(rs[e]) || isUnspaced(rs[e])) && !svElisionAt(rs, e)
        // Between two unspaced characters a run may open or close ("说‘好’"): it is a candidate for both.
        if pending == i {
            pending = nil
            if closer {
                closers.append(i..<e)
                if opener { openers.append(i..<e) }
                if let m = markFrom(e) { pending = m } else { flush() }
                i = e
                continue
            }
            flush()
        }
        if closer { closers.append(i..<e) }
        if opener { openers.append(i..<e) }
        i = e
    }
    flush()
    for i in rs.indices where dropped[i] { rs[i] = "\"" }
    return (rs, dropped)
}

/// Unspoken punctuation that may stand between quoted words and their closing mark ("hello!’"): anything unspoken but
/// a space or another apostrophe.
func svInnerPunct(_ r: Unicode.Scalar) -> Bool { svUnspokenPunct.contains(r) && !isApostrophe(r) }

// --- transcript side: normalisation only ---

/// A transcript token. `link` marks a token joined to the next by a hyphen ("e-mail"): the matcher lets a script
/// word equal to the joined letters consume the whole chain. `lead` and `trail` count the dropped marks
/// (``svUnquote(_:)``) beside a word that may have been apostrophes: the token may also be read with them.
struct SvTok {
    let text: String
    var link = false
    var lead = 0
    var trail = 0
}

func svTranscriptTokens(_ s: String, _ language: String) -> [SvTok] {
    var out: [SvTok] = []
    for t in svLex(svPre(s), hanNumbers: false) {
        let text = svNormApostrophes(svLower(t.text, language))
        if t.kind == .word {
            let parts = fieldsFunc(text, svJoiner)
            for (i, part) in parts.enumerated() {
                out.append(
                    SvTok(
                        text: part, link: i < parts.count - 1, lead: i == 0 ? t.optLead : 0,
                        trail: i == parts.count - 1 ? t.optTrail : 0))
            }
        } else if t.kind == .number {
            let percent = text.hasSuffix("%")
            let body = percent ? String(text.dropLast()) : text
            if svCommaList(body, language) {
                // "1,2,3" is three numbers, cut as the script side cuts it; commas are not spoken
                for p in split(body, ",") { out.append(SvTok(text: p)) }
            } else {
                out.append(SvTok(text: body))
            }
            if percent { out.append(SvTok(text: "%")) }
        } else {
            out.append(SvTok(text: text))
        }
    }
    return out
}
