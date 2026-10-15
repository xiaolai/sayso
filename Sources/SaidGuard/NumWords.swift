// English number words, as the Go package `numwords` spells them; the said guard's readings use them.
enum NumWords {
    static let ones = [
        "zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve",
        "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen",
    ]
    static let tens = ["", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"]
    static let scales: [(v: UInt64, name: String)] = [
        (1_000_000_000_000, "trillion"), (1_000_000_000, "billion"), (1_000_000, "million"), (1000, "thousand"),
    ]

    /// Numbers read as cardinals; longer digit strings are read digit by digit, as are strings with a leading zero.
    static let maxCardinal: UInt64 = 999_999_999_999_999

    /// `n` as words, one word per token: 42 is "forty", "two".
    static func cardinal(_ n: UInt64) -> [String] {
        if n < 20 { return [ones[Int(n)]] }
        if n < 100 {
            var w = [tens[Int(n / 10)]]
            if n % 10 != 0 { w.append(ones[Int(n % 10)]) }
            return w
        }
        if n < 1000 {
            var w = [ones[Int(n / 100)], "hundred"]
            if n % 100 != 0 { w += cardinal(n % 100) }
            return w
        }
        for sc in scales where n >= sc.v {
            var w = cardinal(n / sc.v) + [sc.name]
            if n % sc.v != 0 { w += cardinal(n % sc.v) }
            return w
        }
        preconditionFailure("unreachable")
    }

    /// An ASCII digit string read one digit at a time.
    static func digits(_ ds: String) -> [String] {
        ds.unicodeScalars.map { ones[Int($0.value) - 0x30] }
    }

    private static let ordinalIrregular = [
        "one": "first", "two": "second", "three": "third", "five": "fifth", "eight": "eighth", "nine": "ninth",
        "twelve": "twelfth",
    ]

    /// A cardinal reading as its ordinal: "forty", "two" becomes "forty", "second".
    static func ordinal(_ words: [String]) -> [String] {
        var last = words[words.count - 1]
        if let irregular = ordinalIrregular[last] {
            last = irregular
        } else if last.hasSuffix("y") {
            last = String(last.dropLast()) + "ieth"
        } else {
            last += "th"
        }
        return Array(words.dropLast()) + [last]
    }

    private static let smallValues: [String: Int] = {
        var m: [String: Int] = [:]
        for (i, o) in ones.enumerated() { m[o] = i }
        for (i, t) in tens.enumerated() where !t.isEmpty { m[t] = i * 10 }
        return m
    }()

    /// The value of a word in zero..nineteen or a tens word.
    static func smallValue(_ w: String) -> Int? { smallValues[w] }
}
