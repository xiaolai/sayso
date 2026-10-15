import Foundation

// The locale's spell-out (ICU, through NumberFormatter) as further readings, for the languages of
// `saidGuardSpellOutLanguages`. Every reading it adds is the locale's own spelling of a value read strictly from the
// script, so it cannot drop a sign, a fraction or a digit:
//   - a number in digits is read only if formatting its value in the locale gives back exactly what the script wrote
//     (grouped or not), so "1,000" is a thousand in Hindi and is not a number at all in Portuguese, where it would be
//     one with three decimal zeros written oddly;
//   - a run of words is a number only if it is exactly the locale's spelling of a number in its table of spellings
//     (`numberTables`). ICU's own parser is not used: leniently it accepts words that are not spellings, and a failing
//     lenient parse costs milliseconds, which a long script would multiply.
// Go has no spell-out; there these readings are absent, and the fixtures that need them require `spellout:<lang>`.

final class SpellOut: @unchecked Sendable {
    private let lock = NSLock()
    private let language: String
    private let spell = NumberFormatter()
    private let grouped = NumberFormatter()
    private let plain = NumberFormatter()
    /// The locale's separators, normalised as the guard normalises text (French groups with U+202F, which is a space).
    private let groupSeparator: String
    private let decimalSeparator: String
    private var tables: (values: [String: Decimal], vocabulary: Set<String>)?

    /// Bounds a value read from digits; larger numbers are as written only.
    private static let maxIntegerDigits = 15
    private static let maxFractionDigits = 6
    /// The most words a spelled number is read across.
    private static let maxPhraseWords = 8

    private init(language: String) {
        self.language = language
        let locale = Locale(identifier: language)
        spell.locale = locale
        spell.numberStyle = .spellOut
        for f in [grouped, plain] {
            f.locale = locale
            f.numberStyle = .decimal
            f.maximumFractionDigits = Self.maxFractionDigits
            f.minimumFractionDigits = 0
        }
        plain.usesGroupingSeparator = false
        groupSeparator = svPre(grouped.groupingSeparator ?? "")
        decimalSeparator = svPre(grouped.decimalSeparator ?? "")
    }

    private static let cache = Cache()

    private final class Cache: @unchecked Sendable {
        let lock = NSLock()
        var byLanguage: [String: SpellOut] = [:]
    }

    /// The spell-out for `language`, or nil when its base language has none.
    static func forLanguage(_ language: String) -> SpellOut? {
        guard saidGuardSpellOutLanguages.contains(baseLanguage(language)) else { return nil }
        cache.lock.lock()
        defer { cache.lock.unlock() }
        if let s = cache.byLanguage[language] { return s }
        let s = SpellOut(language: language)
        if cache.byLanguage.count < 64 { cache.byLanguage[language] = s }  // tags come from callers; bound the cache
        return s
    }

    /// The tokens a transcript of `text` has.
    private func tokens(_ text: String) -> [String] { svTranscriptTokens(text, language).map(\.text) }

    /// The spelled tokens of a number written in digits (`body`, without its sign), or nil when the script did not
    /// write a number the locale formats that way.
    func spelled(digits body: String, sign: String) -> [String]? {
        // Negating zero leaves it unsigned, so "-0" would be spelled without its sign: as written only.
        guard let v = value(digits: body), !(sign == "-" && v.isZero) else { return nil }
        lock.lock()
        let s = spell.string(from: NSDecimalNumber(decimal: sign == "-" ? -v : v))
        lock.unlock()
        guard let s else { return nil }
        let t = tokens(s)
        return t.isEmpty ? nil : t
    }

    /// The value the locale writes as `body`, grouped or not; nil for anything else.
    private func value(digits body: String) -> Decimal? {
        var canonical = ""
        var intDigits = 0
        var fracDigits = 0
        var seenDecimal = false
        for r in body.unicodeScalars {
            if svDigit(r) {
                canonical.unicodeScalars.append(r)
                if seenDecimal { fracDigits += 1 } else { intDigits += 1 }
            } else if String(r) == groupSeparator && !seenDecimal {
                continue
            } else if String(r) == decimalSeparator && !seenDecimal {
                canonical.append(".")
                seenDecimal = true
            } else {
                return nil
            }
        }
        guard intDigits > 0, intDigits <= Self.maxIntegerDigits, fracDigits <= Self.maxFractionDigits,
            !seenDecimal || fracDigits > 0, let v = Decimal(string: canonical, locale: Locale(identifier: "en_US_POSIX"))
        else { return nil }
        let n = NSDecimalNumber(decimal: v)
        lock.lock()
        let forms = [grouped.string(from: n), plain.string(from: n)]
        lock.unlock()
        return forms.contains { $0.map(svPre) == body } ? v : nil
    }

    /// A number the locale groups with spaces ("1 000" in French), which the lexer sees as several numbers: one unit
    /// read as written or spelled, and how many raws it spans. nil when the raws at k are not one such number.
    func groupedNumberUnit(_ raws: [SvRaw], _ k: Int, _ language: String) -> (SvUnit, Int)? {
        guard groupSeparator == " " else { return nil }
        var j = k + 1
        // A value of at most maxIntegerDigits digits has at most six groups, which bounds the scan.
        while j < raws.count && j - k < 6 && raws[j].kind == .number && raws[j].gap == " " {
            let d = Array(raws[j].text.unicodeScalars)
            guard d.count >= 3, d[0..<3].allSatisfy(svDigit), d.count == 3 || !svDigit(d[3]) else { break }
            j += 1
        }
        guard j - k >= 2 else { return nil }
        for end in stride(from: j, through: k + 2, by: -1) {
            let written = (k..<end).map { svLower(raws[$0].text, language) }.joined(separator: " ")
            var body = written
            var sign = ""
            if body.hasPrefix("-") {
                body = String(body.dropFirst())
                sign = "-"
            }
            let percent = body.hasSuffix("%")
            if percent { body = String(body.dropLast()) }
            if let spelled = spelled(digits: body, sign: sign) {
                let reading = percent ? cat(lit(spelled), [["%"]]) : lit(spelled)
                return ([lit(tokens(written)), reading], end - k)
            }
        }
        return nil
    }

    /// A number written in the locale's words, at most `maxPhraseWords` words with no punctuation inside, longest
    /// first: one unit read as written or in digits, and how many raws it spans. nil when the words at k are not
    /// exactly the locale's spelling of a number in the table.
    func wordNumberUnit(_ raws: [SvRaw], _ partsOf: [[String]], _ k: Int) -> (SvUnit, Int)? {
        let (values, vocabulary) = numberTables()
        var parts: [String] = []
        var ends: [Int] = []  // ends[l-1]: the words in the first l raws
        var j = k
        while j < raws.count && j < k + Self.maxPhraseWords && word(raws, j) && (j == k || !raws[j].punctBefore) {
            let p = partsOf[j]
            // Every spelling is made of the table's words, so a phrase reaching any other word is no longer a number.
            if p.isEmpty || !p.allSatisfy(vocabulary.contains) { break }
            parts += p
            ends.append(parts.count)
            j += 1
        }
        for l in stride(from: ends.count, through: 1, by: -1) {
            let phrase = Array(parts[..<ends[l - 1]])
            guard let v = values[phrase.joined(separator: " ")] else { continue }
            let n = NSDecimalNumber(decimal: v)
            lock.lock()
            let forms = [plain.string(from: n), grouped.string(from: n)]
            lock.unlock()
            var unit: SvUnit = [lit(phrase)]
            for f in forms {
                guard let f else { continue }
                let reading = lit(tokens(f))
                if !reading.isEmpty && !unit.contains(reading) { unit.append(reading) }
            }
            return (unit, l)
        }
        return nil
    }

    /// The locale's spellings of every number to 10,000, the thousands to 100,000, and the powers up to a trillion,
    /// and every word they use. A number beyond the table, written in words, is read as written only: a false
    /// mismatch, never a false acceptance. Built once per language, in about a tenth of a second.
    private func numberTables() -> (values: [String: Decimal], vocabulary: Set<String>) {
        lock.lock()
        defer { lock.unlock() }
        if let tables { return tables }
        var values: [Decimal] = (0...10_000).map { Decimal($0) }
        values += stride(from: 11_000, through: 100_000, by: 1000).map { Decimal($0) }
        values += [1_000_000, 1_000_000_000, 1_000_000_000_000].map { Decimal($0) }
        var byKey: [String: Decimal] = [:]
        var vocabulary: Set<String> = []
        for v in values {
            guard let s = spell.string(from: NSDecimalNumber(decimal: v)) else { continue }
            let t = tokens(s)
            vocabulary.formUnion(t)
            let key = t.joined(separator: " ")
            if byKey[key] == nil { byKey[key] = v }
        }
        let built = (byKey, vocabulary)
        tables = built
        return built
    }
}
