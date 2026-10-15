# Said guard (`VER-03`, `VER-05`)

`Matches(said, spoken, language)` (Go: `saidguard.Matches`). The question it answers: *did the model claim to read
exactly this script?* `said` is the model's own transcript of its audio, `spoken` the script, `language` a BCP 47
tag. Cases: `fixtures/said-guard.json`, listed in `fixtures/MANIFEST.json`. Both implementations export the guard's
revision (Go `saidguard.Version`, Swift `saidGuardVersion`, now 5), raised whenever a change makes the guard refuse a
reading it accepted, so a cache of accepted audio keyed on it is not reused across such a change.

**Threat model.** The guard's adversary is a TTS model and an
ASR model, not a person crafting input. It must reject the errors those models make:
- a truncated or skipped sentence;
- an added word: a greeting, an answer, or a hallucination;
- a substituted word or a different number;
- a dropped sign, fraction, unit or spoken symbol;
- the wrong language.

Those classes are closed by construction (the invariants below), and a fixture for any of them that fails is a
blocker. Orthographic ambiguity is a different matter: is `CAT` a code or a shouted word, is `Dr` a doctor or a
drive, is `co-op` the same word as `coop`. The context gates resolve it only for common cases, and the rest is a
documented residual. A false acceptance there needs the TTS to make that one spelling-driven mistake, and the
mistake must also coincide with a generated reading. Evidence beyond the fixtures is empirical:
- identity on every real script: a script must match itself;
- measured false-mismatch rates on real TTS → ASR recordings.

A new residual case is added as a fixture with its `note`; it does not reopen the design.

**Design: generate from the script, match the transcript.** Normalising both sides with one rule set and comparing
the results was tried and abandoned: every rewrite rule could map two different texts to one form.

1. **Both sides** get only normalisation: NFKC and width folding; `\p{Cf}` deleted except U+200B, which is a space;
   `\p{Me}` and variation selectors deleted; Unicode decimal digits to ASCII at scalar level; lower case by the
   language's rules (`tr`, `az`); apostrophes unified and **kept**, except a pair of single quotation marks around
   words (`Say ‘hello’.`, `'yes'`), which is unspoken; an unpaired mark is an apostrophe (`’tis`, `students’`). **Only unspoken punctuation is deleted**, and that
   set is closed (`. , ; : ! ? " ( ) [ ] { } … – — / \ | _ ~ ·`, CJK and typographic quotes and brackets, emoji and
   other pictographs). Every other non-letter, non-digit character (`% + = & @ ° − × ÷ < > ≠ # * ′ ^ $ € …`) is a token
   on both sides, so a transcript that adds or drops one does not match. A `%`
   written against a number is split off; a superscript digit is `^` and the digit before NFKC can fold it (`2³` is
   not `23`); a `-` between numbers, the left one possibly ending in `%` or an ordinal suffix (`5% - 2%`, `1st - 3rd`),
   touching the right one (`5 -2`), between Han numerals (`五 - 二`), or before a signed or decimal operand (`5 - +2`,
   `5 - ,2`), is a symbol, silent-able only when tight and outside an expression (`3-4`, `1-0` may be read with no word; `5-2=3`
   and `3 - 4` may not). An expression is a run of numbers and symbols joined only by spaces that holds another
   operator; punctuation ends it (`1-0. #1` holds none). A comma list (`1,2,3`, also `1,2,3%`) is cut into numbers on
   both sides. A number keeps its
   sign (start, after space or punctuation, before `.5` or `,5`, or an exponent `1e-3`), its `. , / :` between digits and an
   ordinal suffix; a leading `,` starts a number only after a space (`,5` is a French decimal, `Yes,3` is a word and a
   number). Only `- ‐ ‑` join words (`e-mail`); dashes `– —` separate them. A letter next to a digit is a token
   boundary; each character of an unspaced script (Han, Hiragana, Katakana, Thai, Lao, Khmer, Myanmar) is a token.
   A hyphen joins after a combining mark too (`हिन्दी-भाषा`). `e.g.` and `i.e.` are one word unless their second letter
   is a capital (`E.G.` is initials). A side with a run of more than 29 non-starters (canonical combining class above
   zero, or Hangul medial and final jamo) in its compatibility decomposition never matches: normalisers differ
   beyond 30 (Go's inserts U+034F, Foundation's does not), so no implementation decides such a run.
2. **The script only** is cut into units, and each unit lists its correct readings. Three invariants hold by
   construction, not by fixture:
   - **A number's every reading says its whole value**: sign, magnitude, fraction, ordinal suffix and percent are
     parsed once and every digit or word form carries all of them, so no alternative can drop one (`-1,000` ≠
     `1000`, `1,000th` ≠ `1000`, `01.5` ≠ `zero one`). Digits are validated before they are read; a malformed number
     (`0.1,000`) is as written.
   - **A Han numeral is a number only if it is a spelling of its own value**: the parser computes a value, then the
     run must equal one of that value's accepted spellings (standard; 两 for 二 before 百千万亿; 一十; 〇 for 零; the
     colloquial dropped last unit `一百一`, `两万五`). Repeated or out-of-order units (`一亿一亿`), a leading or
     place-skipping-nothing 零 (`零一百`, `一百零二十`) and overflow fail the round trip.
   - **A context-dependent reading needs its context**, stated per row below; an abbreviation with no context
     expands to nothing.

   | Unit | Readings |
   |---|---|
   | number, `en` | as written (always, even when grouped beyond a cardinal); with or without thousands commas, also beyond the cardinal range (a grouped number is never digit by digit); cardinal words with no, one or every British "and", and "a" for a leading one before a scale word; year pairs for ungrouped four-digit 1000–2099; digit by digit for four or more ungrouped integer digits (`1234.5` = `one two three four point five`), a leading zero or more than 15 digits, with "point" and digits for a fraction; ordinals in digits (`21st`, only with the right suffix) or words; percent as `%` or "percent"; a sign as `-`, "minus" or "negative"; "zero" also "oh" or "o" in digit readings; a comma list (`1,2,3`) is several numbers |
   | number, `zh` | as written; commas; every accepted Han spelling of its value up to 9,999,999,999,999,999 (`200` → `二百`/`两百`, `2` → `两`, `1000000000000000` → `一千万亿`; 两 only as a whole coefficient, so not `十两万`) |
   | negative zero, any language | `-0` as written, and in `en` with "minus" or "negative"; never a spell-out, which would drop the sign |
   | number, another language | as written; the locale's spell-out where the implementation has one (Swift: ICU), else none (`requires: spellout:<lang>`); `,` and `.` are not interpreted (they are locale-specific) |
   | number with `/` or `:` | as written only |
   | Han numeral run, `zh` | as written; if it round-trips (above), its value in digits, with commas, and its other spellings. A run of digits only is a digit string (`二〇二四` = `2024`) when it has one digit or three or more without 两; two digits are an approximation (`两三天`, `三五天`) and have no digit reading |
   | English number in words | `[minus] cardinal [point digits] [percent]`, or a cardinal ending in an ordinal word (`twenty-first`, also `zeroth`), parsed strictly as the longest prefix of at most 48 words with no punctuation inside: as written, plus every reading of the number it names (`forty-two` = `42`, `minus seven` = `-7`, `three point five percent` = `3.5%`). "zero" is a cardinal only alone (`one thousand zero` is two numbers), and "and" only before a number under the scale word before it (`one hundred and thousand` is not a number) |
   | `oh` / `O`, `en` | `oh`, `o`, `zero`, `0`, only in a run of digits and number words with no punctuation inside (`oh oh seven`, `X O 4`; not `Oh, one more`) |
   | run of single letters | spaced; joined (`I B M` = `IBM`) when it has three letters or more, or none of `a`, `I` (`a T` ≠ `at`) |
   | all-capital code | letter by letter if 2–3 letters, or up to 6 with no vowel (`HTML`), or up to 6 against a number (`JIRA-10442`); never a common short word in capitals (`ON`, `NO`, `GO`: a closed list, `svCapsWords`), and never beside a capital word (`STOP NOW` is shouting; `API URL` are two codes) |
   | 2–3 letters against a number | letter by letter (`mp3`, `Nf3`) |
   | `Mr Mrs Ms Dr`, `en` | expanded only with the capital and before a capitalised word; `Jr Sr` after one |
   | `St`, `Dr` after a capitalised word in the same sentence, `en` | "street", "drive" (`Main St.`, `Take Oak Dr.`; not `Alice. Dr Smith`); `St` before a capitalised word and not after one is "saint" |
   | `Doctor Mister Missus Saint` before a capitalised word | also the abbreviation (`Doctor Smith` = `Dr. Smith`) |
   | `vs`, `etc`, `e.g.`, `i.e.`, `en` | expanded only in lower case after the first letter (`VS Code`, `E.G. White` are not); `for example`, `et cetera`, `that is` also abbreviated |
   | spoken symbol, `en` | as written, or its names (`svSymbolNames`: `percent`, `plus`, `equals`, `minus`/`to`, `times`, `divided by`, `less than`, `not equal to`, `number`, `star`, `prime`, `to the power of`, `squared`/`cubed` for `^2`/`^3`, which may also be read `to the power of` with the exponent in words or digits, …); unnamed symbols (currency) and other languages as written |
   | hyphenated word | split, or joined (`e-mail` = `email`) |
   | lowercase `a.b.c` | as written, or with "dot" between every part (`www dot example dot com`); a capital (`Dr.Smith`, `end.Next`) is not a domain. Recognised before number words (`one.com`); a label may be one letter (`x.com`), and a hyphenated label is cut as the transcript cuts it (`e-mail.com`). A run of single letters (`a.b.c`) is a spelled code instead |
   | any other word or character | as written |

3. **Match** iff the script has at least one unit and the transcript's tokens are one reading per unit, in order. A
   script token also matches a hyphen chain in the transcript that joins to it (`email` = `e-mail`, up to 8 parts),
   compared after normalising the joined text again (joined jamo `ᄀ-ᅡ` compose to `가`).
   The matcher is a bitset simulation: bit *j* of the frontier means the units so far can end at token *j*, and each
   step ANDs it with the sparse start positions of an accepted token and shifts. Work follows the frontier's span, not
   a memo of every (unit, position) pair. Inputs above 64 KiB per side never match.

The transcript is never parsed into numbers or abbreviations, and it deletes no spoken symbol. A false acceptance
therefore needs either a generated reading that is not a correct reading of the script, or a deleted character that
was spoken. The table and the separator rule are the whole attack surface.

Accepted false mismatches (each a regeneration, never a false acceptance): `사과3개` ≠ `사과삼개`, `21` ≠ `ein und
zwanzig`, `1.000` ≠ `tausend`, `1/2` ≠ `one half`, a script number word read back as digits in a language without
spell-out, a reverse year (`nineteen ninety-nine` ≠ `1999`), `Lakers VS Celtics` ≠ `versus` (capitals read as a code),
`Qxe7+` ≠ `Q X E7` (the spoken `+` is missing; Checkmate's TTS scripts contain no symbols), `007th` ≠ `zero zero
seventh`, currency in words (`$5` ≠ `five dollars`: the reading reorders, deferred until a script needs it; `$` is a
token, so an omitted currency is detected), `pages 3 - 4` ≠ `pages 3 4` (a spaced dash is spoken), a two-digit Han
string (`一二` ≠ `12`), a Japanese numeral in kanji read back as digits (`四十二` ≠ `42`: Japanese numerals also
spell words, `一緒`, `七夕`, so a run is not read as a value), and a character newer than the platform's Unicode
tables (a Unicode 16 digit on macOS 14 reads as a symbol in Swift; the same table difference can only refuse, since
an unknown character is a token that matches only itself). An emphasised short word missing from `svCapsWords` is the opposite case: it is accepted letter
by letter, a residual false acceptance (see the threat model).

Accepted residual false acceptances, each with its mechanism in the fixture `note`: a hyphenated compound read as its
closed form (`co-op` = `coop`, `re-cover` = `recover`): the guard compares words, not pronunciation, and correct
recordings of spelling-variant compounds come back from ASR in either form; `Oh one more thing` without a comma =
`zero one more thing`; `St` between a sentence's capitalised first word and a capital (`Main St. Next turn left.`,
`Visit St. John`) reads "saint", as `Visit St. John` must, so `Main saint` is accepted and `Main street` refused there.
