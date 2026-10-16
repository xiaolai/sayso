# Said guard (`VER-03`, `VER-05`)

`Matches(said, spoken, language)` (Go: `saidguard.Matches`). The question it answers: *did the model claim to read
exactly this script?* `said` is the model's own transcript of its audio, `spoken` the script, `language` a BCP 47
tag. Cases: `fixtures/said-guard.json`, listed in `fixtures/MANIFEST.json`. Both implementations export the guard's
revision (Go `saidguard.Version`, Swift `saidGuardVersion`, now 12), raised whenever a change makes the guard refuse a
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
   language's rules (`tr`, `az`); apostrophes unified and **kept**, except single quotation marks around words
   (`Say ‘hello’.`, `'yes'`), which are unspoken. Which mark closes which is not decided: in `‘the students’ books’`
   the middle mark may be a possessive. An opener stands at a word start; a closer at a word end, possibly after the
   punctuation that ends the quoted words (`‘hello!’`, `‘yes,’`, `‘done.’`); an opener may also stand before a digit
   (`‘5’`). An unspaced character is a word of its own, so it is a word boundary on either side, as a space is: an
   opener may follow one and a closer precede one, a quoted run of them pairs (`他说‘好’。` = `他说好。`), and a mark
   between two of them may open or close. Within one sentence, which ends at its final punctuation or, where closers follow that, right after them
   (`‘Wait!’ The students’ books.` is two sentences), every opener with a closer after it and every closer with an
   opener before it may be a quotation mark, and each side reads the word beside such a mark both without it and with
   it as an apostrophe; the transcript selects. Each mark is a local choice of its word, so the readings stay linear in
   the marks. Any other mark is an apostrophe (`’tis`, `students’` with no opener before it), and so is a mark before
   a leading elision, which is never an opener (`svElisions`, a closed list: `’tis`, `’twas`, `’em`, `’n’`, `’cause`,
   …), so `’Tis the students’ books` keeps both. An elision is a whole word: `n` in `‘n-gram’` is none. A `.` ends no
   sentence when a letter or digit follows it (`e.g`, `3.5`, `x.com`) or when it closes `Mr Mrs Ms Dr St Jr Sr vs e.g.
   i.e.` (`Say ‘Dr. Smith’.` is one quotation); `etc.` ends one. An apostrophe that is no quotation mark after a number
   (`5'`, `5’10`, `21st’`, `五’`) or beside an unspaced character (`好’`, `’好`), as a word keeps its own (`’tis`), a
   second one after a number (`5''`), and a `"` after a number that follows such a mark
   (`5'10"`) are spoken symbols on both sides; any other `"` is an unspoken quotation mark (`"42"`). **Only unspoken punctuation is deleted**, and that
   set is closed (`. , ; : ! ? " ( ) [ ] { } … – — / \ | _ ~ ·`, CJK and typographic quotes and brackets, emoji and
   other pictographs). Every other non-letter, non-digit character (`% + = & @ ° − × ÷ < > ≠ # * ′ ^ $ € …`) is a token
   on both sides, so a transcript that adds or drops one does not match. A `%`
   written against a number is split off; a superscript digit is `^` and the digit before NFKC can fold it (`2³` is
   not `23`); a `-` between numbers, the left one possibly ending in `%` or an ordinal suffix (`5% - 2%`, `1st - 3rd`),
   touching the right one (`5 -2`), between Han numerals (`五 - 二`), or before a signed or decimal operand (`5 - +2`,
   `5 - ,2`), is a symbol, silent-able only when tight and outside an expression (`3-4`, `1-0` may be read with no word; `5-2=3`
   and `3 - 4` may not). An expression is a run of numbers and symbols joined only by spaces that holds another
   operator; punctuation ends it (`1-0. #1` holds none). Every other `-` and `−` takes its role from the dash table
   below; a free dash is a symbol on both sides, and a silent one may be read with no word except in a run of words,
   unspaced characters, numbers and symbols joined only by spaces that holds an arithmetic operator `= + − × ÷ < > ≠ *
   ^` (`x - y = z`, `甲 - 乙 = 丙`). A Han numeral outside `zh`, lexed as an unspaced character, is still a number in a numeric run (`五-二=三` in
   `ja` keeps its minus). A comma list (`1,2,3`, also `1,2,3%`) is cut into numbers on
   both sides. A number keeps its
   sign (the dash table: `-5`, `- 5`, `-.5`, `-,5`, or an exponent `1e-3`), its `. , / :` between digits and an
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
   | English number in words | `[minus] cardinal [point digits] [percent]`, or a cardinal ending in an ordinal word (`twenty-first`, also `zeroth`), parsed strictly as the longest prefix of at most 48 words with no punctuation inside: as written and in every ordinary reading of its words (`TEN` = `T E N`, `forty-two` = `fortytwo`; as written only beyond 64 combined readings), plus every reading of the number it names (`forty-two` = `42`, `minus seven` = `-7`, `three point five percent` = `3.5%`). "zero" is a cardinal only alone (`one thousand zero` is two numbers), and "and" only before a number under the scale word before it (`one hundred and thousand` is not a number) |
   | `oh` / `O`, `en` | `oh`, `o`, `zero`, `0`, only in a run of digits and number words with no punctuation inside (`oh oh seven`, `X O 4`; not `Oh, one more`) |
   | run of single letters | spaced; joined (`I B M` = `IBM`) when it has three letters or more, or none of `a`, `I` (`a T` ≠ `at`) |
   | all-capital code | letter by letter if 2–3 letters, or up to 6 with no vowel (`HTML`), or up to 6 against a number (`JIRA-10442`); never a common short word in capitals (`ON`, `NO`, `GO`: a closed list, `svCapsWords`), and never beside a capital word (`STOP NOW` is shouting; `API URL` are two codes) |
   | 2–3 letters against a number | letter by letter (`mp3`, `Nf3`) |
   | `Mr Mrs Ms Dr`, `en` | expanded only with the capital and before a capitalised word; `Jr Sr` after one |
   | `St`, `Dr` after a capitalised word in the same sentence, `en` | "street", "drive" (`Main St.`, `Take Oak Dr.`; not `Alice. Dr Smith`); `St` before a capitalised word and not after one is "saint" |
   | `Doctor Mister Missus Saint` before a capitalised word | also the abbreviation (`Doctor Smith` = `Dr. Smith`) |
   | `vs`, `etc`, `e.g.`, `i.e.`, `en` | expanded only in lower case after the first letter (`VS Code`, `E.G. White` are not); `for example`, `et cetera`, `that is` also abbreviated |
   | spoken symbol, `en` | as written, or its names (`svSymbolNames`: `percent`, `plus`, `equals`, `minus`/`to`, `times`, `divided by`, `less than`, `not equal to`, `number`, `star`, `prime`, `to the power of`, `squared`/`cubed` for `^2`/`^3`, which may also be read `to the power of` with the exponent in words or digits, …); a measurement mark `'` or `′` right after a number is also `feet`, `foot`, `minutes`, `minute`, and two of them (`5''`, `30″`, which NFKC folds to `′′`) or the `"` after feet (`5'10"`) `inches`, `inch`, `seconds`, `second`, never silent; unnamed symbols (currency) and other languages as written |
   | hyphenated word | split, or joined (`e-mail` = `email`) |
   | lowercase `a.b.c` | as written, or with "dot" at any of its dots (`www dot example dot com`; a transcript that writes a dot, `www dot example.com`, already reads as no "dot" there, so a per-dot choice accepts nothing the written form did not); a capital (`Dr.Smith`, `end.Next`) is not a domain. Recognised before number words (`one.com`); a label may be one letter (`x.com`), and each hyphenated label is split or joined on its own (`e-mail.long-term.com` = `email dot long term dot com`). One unit a label and one a dot, so the readings are linear in the labels. A run of single letters (`a.b.c`) is a spelled code instead |
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

**The dash table.** One rule decides every `-` and `−` that is not inside a word (`e-mail`). Its *operand* is the
first character after it past any spaces, so spaces between a sign and its number never decide (`- 5` is `-5`); what
precedes it is the character right before it. Before a word, though, a spaced dash is prose punctuation (`Monday -
Friday`, a list mark `- item`), so a sign before a letter must touch it. `−` is spoken in every row.

| Before the dash | Operand (past spaces) | Role | Readings, never silent unless stated |
|---|---|---|---|
| a number (digits, `%`, ordinal suffix, Han numeral), spaces allowed | a number, possibly signed | minus between numbers | `-`, `minus`, `to`; silent only touching both numbers outside a numeric expression (`3-4`) |
| the start, a space, punctuation or an unspaced character | a digit or `.5`/`,5` | sign, part of the number (`-5`, `- 5`, `气温-5度`) | the signed number's readings |
| the start, a space, punctuation or an unspaced character | a Han numeral | the numeral's sign (`-五`, `- 五`, `−五`) | `-`/`−` then the numeral's readings, or `-5` |
| a letter, digit or mark, touching | a digit, touching | a code's joiner (`JIRA-10442`) | silent |
| a letter, digit or mark | a number, not touching on both sides (`x- 5`, `A-五`) | spoken dash | `-`, `minus`, `to` |
| the start, a space or punctuation | a letter or unspaced character, touching (`-x`) | sign | `-`, `−`, `minus`, `negative`; never `to` |
| an unspaced character | a letter or unspaced character, touching (`北京-上海`) | joins two words | as written, or silent |
| a letter, digit or mark | a letter, touching | inside a word (`e-mail`) | the word split or joined |
| anything | a letter or unspaced character past a space, nothing, or punctuation (`Monday - Friday`, `- item`, `Wait -`) | separator | as written, `minus`, `to`, or silent |

**Runs of marks.** Every rule that looks at marks reads a run of consecutive apostrophes of any length as one, found
in one reverse pass as row 1's punctuation is: a run opens or closes a quotation as a whole (`''yes''` = `yes`), a
dropped run beside a word may be read as that many apostrophes, a run before a letter stays with the word (`’’tis`),
and every kept mark of a run after a number or beside an unspaced character is a symbol (`五’’’` ≠ `五’’`).

Accepted false mismatches (each a regeneration, never a false acceptance): `사과3개` ≠ `사과삼개`, `21` ≠ `ein und
zwanzig`, `1.000` ≠ `tausend`, `1/2` ≠ `one half`, a script number word read back as digits in a language without
spell-out, a reverse year (`nineteen ninety-nine` ≠ `1999`), `Lakers VS Celtics` ≠ `versus` (capitals read as a code),
`Qxe7+` ≠ `Q X E7` (the spoken `+` is missing; Checkmate's TTS scripts contain no symbols), `007th` ≠ `zero zero
seventh`, currency in words (`$5` ≠ `five dollars`: the reading reorders, deferred until a script needs it; `$` is a
token, so an omitted currency is detected), `pages 3 - 4` ≠ `pages 3 4` (a spaced dash is spoken), a two-digit Han
string (`一二` ≠ `12`), a Japanese numeral in kanji read back as digits (`四十二` ≠ `42`: Japanese numerals also
spell words, `一緒`, `七夕`, so a run is not read as a value), a free dash the transcript adds (`Monday, Friday` ≠
`Monday - Friday`), a list mark before a number (`- 5 apples` ≠ `5 apples`: the dash table reads it as a sign), a measurement mark in another language's words (`5'` ≠ `5 pieds` in `fr`: symbol names are English only), a sign
before a Han numeral read as `负` (`-五` ≠ `负五`: `zh` has no spell-out), a number's mark kept by the transcript where the script's
was a quotation mark (`‘42’` ≠ `42’`), and a character newer than the platform's Unicode tables (a Unicode 16 digit on macOS 14 reads as a symbol in Swift; the same table difference can only refuse, since
an unknown character is a token that matches only itself). An emphasised short word missing from `svCapsWords` is the opposite case: it is accepted letter
by letter, a residual false acceptance (see the threat model).

Accepted residual false acceptances, each with its mechanism in the fixture `note`: a hyphenated compound read as its
closed form (`co-op` = `coop`, `re-cover` = `recover`): the guard compares words, not pronunciation, and correct
recordings of spelling-variant compounds come back from ASR in either form; `Oh one more thing` without a comma =
`zero one more thing`; `St` between a sentence's capitalised first word and a capital (`Main St. Next turn left.`,
`Visit St. John`) reads "saint", as `Visit St. John` must, so `Main saint` is accepted and `Main street` refused there.
