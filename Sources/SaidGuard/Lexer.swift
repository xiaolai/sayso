import Foundation

// --- lexer ---

enum SvKind { case word, number, unspacedChar, hanRun, symbol }

struct SvRaw {
    let text: String
    let kind: SvKind
    /// The text between the previous raw and this one.
    let gap: String

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
    if n < rs.count && ((svMinus(rs[n]) && svSignAt(rs, n)) || (rs[n] == "+" && svOperandAt(rs, n + 1))) {
        n += 1  // a signed operand ("5 - -2", "5 - +2")
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

/// A '-' or '−' before a digit, or a decimal point or comma before one ("-.5", "-,5"), is a sign at the start, after a
/// space or punctuation, or as an exponent sign ("1e-3"); after a letter it is a joiner ("JIRA-10442"), and after a
/// number, spaced or not, a minus ("3-4", "5 -2").
func svSignAt(_ rs: [Unicode.Scalar], _ i: Int) -> Bool {
    if !svMinus(rs[i]) || i + 1 >= rs.count { return false }
    if !svDigit(rs[i + 1]) && !((rs[i + 1] == "." || rs[i + 1] == ",") && i + 2 < rs.count && svDigit(rs[i + 2])) {
        return false
    }
    if i == 0 { return true }
    let p = rs[i - 1]
    if (p == "e" || p == "E") && i >= 2 && svDigit(rs[i - 2]) { return true }
    return !svLetter(p) && !svDigit(p) && !isMark(p)
}

func isHanNumeral(_ r: Unicode.Scalar) -> Bool {
    svHanDigit[r] != nil || svHanBigUnit[r] != nil || svHanSmallUnit[r] != nil
}

private let svNumberSeparators = Set(".,/:".unicodeScalars)

func svLex(_ s: String, hanNumbers: Bool) -> [SvRaw] {
    let rs = svUnquote(Array(s.unicodeScalars))
    var out: [SvRaw] = []
    var prevEnd = 0
    func emit(_ kind: SvKind, _ i: Int, _ j: Int) {
        // One sign, however written: "−5" reads as "-5".
        let text = kind == .number && rs[i] == "−" ? "-" + svString(rs[i + 1..<j]) : svString(rs[i..<j])
        out.append(SvRaw(text: text, kind: kind, gap: svString(rs[prevEnd..<i])))
        prevEnd = j
    }
    var i = 0
    while i < rs.count {
        let r = rs[i]
        if svDigit(r) || (svSignAt(rs, i) && !svBetweenNumbers(rs, i))
            || (r == "." && i + 1 < rs.count && svDigit(rs[i + 1]) && (i == 0 || !svDigit(rs[i - 1])))
            || (r == "," && i + 1 < rs.count && svDigit(rs[i + 1]) && (i == 0 || isSpace(rs[i - 1])))
        {
            // A leading "," is a number only after a space (",5" is a French decimal); "Yes,3" is a word and a number.
            var j = i + 1
            while j < rs.count && svDigit(rs[j]) { j += 1 }
            while j + 1 < rs.count && svNumberSeparators.contains(rs[j]) && svDigit(rs[j + 1]) {
                j += 2
                while j < rs.count && svDigit(rs[j]) { j += 1 }
            }
            if j + 1 < rs.count && svDigit(rs[j - 1]) {
                let suf = goLower(svString(rs[j..<j + 2]))
                if (suf == "st" || suf == "nd" || suf == "rd" || suf == "th") && (j + 2 >= rs.count || !svLetter(rs[j + 2])) {
                    j += 2
                }
            }
            if j < rs.count && rs[j] == "%" { j += 1 }
            emit(.number, i, j)
            i = j
        } else if svSpokenSymbol(r) || (svMinus(r) && svBetweenNumbers(rs, i)) {
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
        } else if svLetter(r) || (isApostrophe(r) && i + 1 < rs.count && svLetter(rs[i + 1])) {
            // "e.g." and "i.e." are one word, also at a sentence start ("E.g."). With a capital second letter they are
            // initials ("E.G. White"), lexed letter by letter like any other.
            let m = goLower(svString(rs[i..<min(i + 4, rs.count)]))
            // Not inside a domain ("www.e.g.com"), where its letters are labels.
            if (m.hasPrefix("e.g") || m.hasPrefix("i.e")) && !isUpper(rs[i + 2])
                && (i == 0 || (!svLetter(rs[i - 1]) && rs[i - 1] != "."))
                && !(i + 4 < rs.count && rs[i + 3] == "." && svLetter(rs[i + 4]))
                && (i + 3 >= rs.count || !svLetter(rs[i + 3]))
            {
                emit(.word, i, i + 3)
                i += 3
                if i < rs.count && rs[i] == "." { i += 1 }
                continue
            }
            var j = i
            while j < rs.count {
                let c = rs[j]
                if svLetter(c) || isMark(c) {
                    j += 1
                } else if isApostrophe(c) && (j > i || (j + 1 < rs.count && svLetter(rs[j + 1]))) {
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

private let svQuoteOpeners = Set("([{\"“«".unicodeScalars)
private let svSentenceEndScalars = Set(".!?。".unicodeScalars)

/// The English words written with a leading apostrophe for dropped letters ("’tis", "’em", "’n’"). A mark before one
/// is an apostrophe, never an opening quotation mark: a closed list, as the guard's other word lists.
let svElisions: Set<String> = [
    "tis", "twas", "twere", "twill", "twould", "em", "n", "cause", "cos", "til", "bout", "cept", "neath", "gainst",
    "tween", "twixt", "scuse", "nuff",
]

/// Whether the letters starting at rs[i], in any case, are a whole word of ``svElisions``.
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
    return (i == rs.count || !isMark(rs[i])) && svElisions.contains(b)
}

/// Replaces a pair of single quotation marks around words ("Say ‘hello’.", "'yes'") with an unspoken '"', so the words
/// inside are not read as elisions or possessives. An opener stands at a word start after a space, an opening bracket
/// or the start, and never before a leading elision (``svElisions``: "’Tis the students’ books" holds two apostrophes,
/// not a quotation); a closer at a word end, after any punctuation that ends the quoted words ("‘hello!’", "‘yes,’"),
/// before a space, punctuation or the end; a pair stays within a sentence, and a sentence ends at its final punctuation
/// only where no closer follows it ("‘Wait.’" is one quotation). An unpaired mark is an apostrophe ("’tis", "the
/// students’ books", "rock ’n’ roll").
func svUnquote(_ input: [Unicode.Scalar]) -> [Unicode.Scalar] {
    var rs = input
    var open = -1
    for i in rs.indices {
        let r = rs[i]
        if !isApostrophe(r) {
            if svSentenceEndScalars.contains(r), !svApostropheAfterPunct(rs, i + 1) { open = -1 }
            continue
        }
        let opener = (i == 0 || isSpace(rs[i - 1]) || svQuoteOpeners.contains(rs[i - 1])) && i + 1 < rs.count
            && svLetter(rs[i + 1]) && !svElisionAt(rs, i + 1)
        var k = i - 1
        while k >= 0 && svInnerPunct(rs[k]) { k -= 1 }
        let closer = k >= 0 && (svLetter(rs[k]) || isMark(rs[k]) || svDigit(rs[k]))
            && (i + 1 == rs.count || isSpace(rs[i + 1])
                || (svUnspokenPunct.contains(rs[i + 1]) && !isApostrophe(rs[i + 1])))
        if closer && open >= 0 {
            rs[open] = "\""
            rs[i] = "\""
            open = -1
        } else if opener {
            open = i
        }
    }
    return rs
}

/// Unspoken punctuation that may stand between quoted words and their closing mark ("hello!’"): anything unspoken but
/// a space or another apostrophe.
func svInnerPunct(_ r: Unicode.Scalar) -> Bool { svUnspokenPunct.contains(r) && !isApostrophe(r) }

/// Whether the first scalar from rs[i] on that is not ``svInnerPunct`` is an apostrophe.
func svApostropheAfterPunct(_ rs: [Unicode.Scalar], _ start: Int) -> Bool {
    var i = start
    while i < rs.count && svInnerPunct(rs[i]) { i += 1 }
    return i < rs.count && isApostrophe(rs[i])
}

// --- transcript side: normalisation only ---

/// A transcript token. `link` marks a token joined to the next by a hyphen ("e-mail"): the matcher lets a script
/// word equal to the joined letters consume the whole chain.
struct SvTok {
    let text: String
    var link = false
}

func svTranscriptTokens(_ s: String, _ language: String) -> [SvTok] {
    var out: [SvTok] = []
    for t in svLex(svPre(s), hanNumbers: false) {
        let text = svNormApostrophes(svLower(t.text, language))
        if t.kind == .word {
            let parts = fieldsFunc(text, svJoiner)
            for (i, part) in parts.enumerated() { out.append(SvTok(text: part, link: i < parts.count - 1)) }
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
