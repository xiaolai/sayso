#!/usr/bin/env python3
"""Run the Go and Swift implementations on the same generated inputs and fail on any disagreement.

Usage: tools/differential.py [--seed N] [--count N] [--ops OP,...] [--keep DIR] [--timeout S]

The cases are fixture inputs mutated at one position (an insertion, deletion or replacement drawn from character
classes where Unicode and JSON libraries part ways), JSON documents mutated by value, and random strings from the
same classes. --seed fixes them and --count is the number of cases per operation. Each implementation answers through
its oracle: TestOracle in go/saidguard, go/codexauth and go/conformance (go/internal/oracle), and
Tests/ConformanceTests/OracleTests.swift. A case whose observations differ is a divergence unless ALLOWED documents
it with a sentence of the spec; for each operation the shortest diverging input is printed as a candidate fixture.

Exit status: 0 when every case agrees, 1 on a divergence, 2 when the harness fails (an oracle that does not build,
crashes, misses a case or runs past --timeout).
"""
import argparse
import base64
import bisect
import json
import os
import pathlib
import random
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
import unicodedata

ROOT = pathlib.Path(__file__).resolve().parent.parent
FIXTURES = ROOT / "fixtures"
DEFAULT_SEED = 20261016
DEFAULT_COUNT = 2000  # 26,000 cases: 10 to 30 s on an M-series Mac once both are built
DEFAULT_TIMEOUT = 1800  # seconds per oracle command, a first build included

# Each operation, and the Go package whose TestOracle answers it. Swift answers every one.
OPS = {
    "said.matches": "saidguard", "said.tokens": "saidguard", "han.parse": "saidguard", "jwt.exp": "codexauth",
    "jwt.account": "codexauth", "echo": "conformance", "wire.valid": "conformance", "codex.valid": "conformance",
    "event.decode": "conformance",
    "pcm.encode": "conformance", "pcm.decode": "conformance", "pcm.wav": "conformance", "pcm.validWAV": "conformance",
    "pcm.plausible": "conformance",
}

SAID_LANGUAGES = {"en": ["en", "en", "en", "en-US", "EN", "en_GB"], "zh": ["zh", "zh", "zh", "zh-Hans", "zh-Hant", "zh_TW"]}
SPELLOUT = {"ar", "de", "es", "fr", "hi", "it", "ja", "ko", "pt"}


class Draw:
    """Choices made from random.random() alone: for a given seed Python keeps that sequence across versions, which it
    does not promise for choice, randrange or shuffle."""

    def __init__(self, seed):
        self._r = random.Random(seed)

    def below(self, n):
        return min(int(self._r.random() * n), n - 1)

    def chance(self, p):
        return self._r.random() < p

    def choice(self, seq):
        return seq[self.below(len(seq))]

    def between(self, lo, hi):
        return lo + self.below(hi - lo + 1)


# --- Unicode 15.0 ---

def load_assigned(path=ROOT / "tools" / "unicode-15.0-assigned.txt"):
    """The ranges assigned in Unicode 15.0, as (starts, ends)."""
    starts, ends = [], []
    for line in path.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            a, b = line.split("..")
            starts.append(int(a, 16))
            ends.append(int(b, 16))
    if not starts:
        sys.exit(f"{path}: no ranges")
    return starts, ends


ASSIGNED_15 = load_assigned()


def assigned15(s):
    """Whether every character of s is assigned in Unicode 15.0."""
    starts, ends = ASSIGNED_15
    for ch in s:
        k = bisect.bisect_right(starts, ord(ch)) - 1
        if k < 0 or ord(ch) > ends[k]:
            return False
    return True


# --- character classes ---

# The zero of every block of decimal digits assigned in Unicode 15.0, the five mathematical runs included.
ND_ZEROS = [
    0x30, 0x660, 0x6F0, 0x7C0, 0x966, 0x9E6, 0xA66, 0xAE6, 0xB66, 0xBE6, 0xC66, 0xCE6, 0xD66, 0xDE6, 0xE50, 0xED0,
    0xF20, 0x1040, 0x1090, 0x17E0, 0x1810, 0x1946, 0x19D0, 0x1A80, 0x1A90, 0x1B50, 0x1BB0, 0x1C40, 0x1C50, 0xA620,
    0xA8D0, 0xA900, 0xA9D0, 0xA9F0, 0xAA50, 0xABF0, 0xFF10, 0x104A0, 0x10D30, 0x11066, 0x110F0, 0x11136, 0x111D0,
    0x112F0, 0x11450, 0x114D0, 0x11650, 0x116C0, 0x11730, 0x118E0, 0x11950, 0x11C50, 0x11D50, 0x11DA0, 0x11F50,
    0x16A60, 0x16AC0, 0x16B50, 0x1D7CE, 0x1D7D8, 0x1D7E2, 0x1D7EC, 0x1D7F6, 0x1E140, 0x1E2F0, 0x1E4F0, 0x1E950,
    0x1FBF0,
]

HAN_NUMERALS = "〇零一二两三四五六七八九十百千万亿"

# Units an edit inserts, by class; a unit may be several characters (an emoji sequence, a word).
CLASSES = {
    "ascii": list("abcxyzABCXYZ0123456789 .,;:!?'\"-()[]{}/\\|_~@#$%^&*+=<>`"),
    "marks": ["\u0300", "\u0301", "\u0308", "\u030a", "\u0323", "\u0327", "\u0345", "\u034f", "\u0489", "\u05b4",
              "\u064b", "\u0670", "\u093c", "\u094d", "\u0903", "\u0e31", "\u0e48", "\u0ec8", "\u1ab0", "\u1dc0",
              "\u20d0", "\u20dd", "\u20e3", "\u302a", "\u3099", "\ufe20", "\U0001d165", "\U0001d167",
              "\u0323\u0301", "\u0301" * 15, "\u0301" * 30],
    "jamo": ["\u1100", "\u1161", "\u11a8", "\u115f", "\u1160", "\ud7b0", "\ud7cb", "\u3131", "\uac00"],
    # An LV syllable before a trailing consonant, which NFC composes into one LVT syllable; also through a
    # compatibility jamo that NFKC maps to a trailing one.
    "hangul": [chr(lv) + chr(t) for lv in (0xAC00, 0xB098, 0xD788) for t in (0x11A8, 0x11AA, 0x11C2, 0x3133, 0xFFA3)]
    + [chr(0x1100) + chr(0x1161), chr(0x1100) + chr(0x1161) + chr(0x11A8), chr(0xAC01) + chr(0x11A8)],
    # Parts that compose once joined: jamo, a syllable and a trailing consonant, a mark after the hyphen; a
    # composition of a composed character (Kannada).
    "joins": ["-".join(chr(c) for c in pair) for pair in ((0x1100, 0x1161), (0xAC00, 0x11A8), (0xAC00, 0x3133))]
    + ["e-" + chr(0x301) + "mail", "a-" + chr(0x308) + "b", "n-" + chr(0x303) + "o", chr(0xCCA) + chr(0xCD5),
       chr(0xCCA) + "-" + chr(0xCD5)],
    # Clusters a grapheme-based comparison reads differently from a scalar-based one: a letter and a mark with no
    # precomposed form, after "e.g" and "i.e" too; letters that prepend to the next cluster; a spacing mark.
    "graphemes": ["e.g" + chr(0x323), "i.e" + chr(0x331), "E.g" + chr(0x323) + ".", "g" + chr(0x323), "q" + chr(0x301),
                  "x" + chr(0x308) + chr(0x323), chr(0xD4E), chr(0x111C2), chr(0x11A3A), "5" + chr(0x903), "%" + chr(0x301),
                  "-" + chr(0x301), "." + chr(0x301)],
    "digits": [chr(z + d) for z in ND_ZEROS for d in range(10)],
    "dashes": list("-‐‑‒–—―−﹘﹣－⸺⸻⁃\u00adー"),
    "apostrophes": list("'’‘ʼ‛′″‴\"“”„‟`´ʻ＇＂")
    + ["''", "’’", "′′"],
    "han": list(HAN_NUMERALS + "壹贰拾佰仟廿卅兆"),
    "fullwidth": list("０１５９ＡＺａｚ％＋－．，：／＝\u3000ｱﾞ"),
    "joiners": ["\u200d", "\u200c", "\u200b", "\u2060", "\ufeff", "\u00ad", "\u180e", "\u061c", "\u200e", "\u202e",
                "\ufe0f", "\ufe0e", "\U000e0100", "\U000e0001", "\U000e0041"],
    "spaces": [" ", "  ", "\u00a0", "\u202f", "\u2007", "\u2009", "\u3000", "\u1680", "\u0085", "\u2028", "\t", "\n",
               "\r", "\x0b", "\x0c"],
    "supersub": list("⁰¹²³⁴⁵⁶⁷⁸⁹⁺⁻₀₁₂½¼¾⅓⁄ⅣⅫ①⑩㉑") + ["\U0001d7ce", "\U0001d7ff"],
    "symbols": list("%+=&@°×÷<>≠#*^$€£¥.,;:!?()[]{}/\\|_~·…•§©®™✓→"),
    "cases": list("İıßẞΣσςǅǈﬁﬀ\u212a\u212b\u2126Ĳŉǰΐ"),
    "unspaced": list("ไทยภาษาລາວខ្មែរမြန်မာひらがなカタカナ中文漢字"),
    "letters": list("aéÉñçøÆœДжαβΓשلسنमह한국"),
    "emoji": ["\U0001f600", "\U0001f44d", "\U0001f3fd", "\U0001f468\u200d\U0001f469\u200d\U0001f467",
              "\U0001f1fa\U0001f1f8", "1\ufe0f\u20e3", "❤\ufe0f", "\U0001fae0"],
    "words": [" - ", "--", "''", "‘", "’tis", "e.g.", "i.e.", "E.G.", "Dr.", "St.", "Mr", "vs", "etc.",
              "1,000", "3.5", "-5", "- 5", "-.5", ",5", "1,2,3", "1e-3", "5'10\"", "5''", "2³", "x.com", "a-b",
              "e-mail", "I B M", "1st", "21st", "%", " , ", "oh", "ten", "TEN", "twenty-one", "one hundred and five",
              "minus", "两万五", "一百一", "-五", "五-二=三"],
}
TEXT_CLASSES = sorted(CLASSES)


def edit(draw, s, classes=TEXT_CLASSES):
    """s with one insertion, deletion or replacement at one position, the unit drawn from one of classes."""
    unit = draw.choice(CLASSES[draw.choice(classes)])
    action = draw.below(3) if s else 0
    if action == 0:
        i = draw.below(len(s) + 1)
        return s[:i] + unit + s[i:]
    i = draw.below(len(s))
    return s[:i] + s[i + 1:] if action == 1 else s[:i] + unit + s[i + 1:]


def random_text(draw, classes=TEXT_CLASSES, most=12):
    return "".join(draw.choice(CLASSES[draw.choice(classes)]) for _ in range(draw.between(1, most)))


# --- JSON with duplicates and numbers as written ---

class Raw(str):
    """JSON text written as is: a number, or a name spelled with escapes."""


class Obj(list):
    """A JSON object as its members in order, so a duplicate name survives."""


def parse_json(text):
    return json.loads(text, object_pairs_hook=Obj, parse_float=Raw, parse_int=Raw, parse_constant=Raw)


def units16(s):
    """The UTF-16 code units of s, as a \\u escape spells them."""
    for ch in s:
        c = ord(ch)
        if c > 0xFFFF:
            yield from (0xD800 + ((c - 0x10000) >> 10), 0xDC00 + ((c - 0x10000) & 0x3FF))
        else:
            yield c


def quote(s, ascii_only=False):
    out = ['"']
    for ch in s:
        c = ord(ch)
        if ch in '"\\':
            out.append("\\" + ch)
        elif c < 0x20 or 0xD800 <= c <= 0xDFFF or (ascii_only and c > 0x7E):
            out.extend("\\u%04x" % u for u in units16(ch))
        else:
            out.append(ch)
    return "".join(out) + '"'


def dump_json(v, ascii_only=False):
    if isinstance(v, Raw):
        return str(v)
    if v is True:
        return "true"
    if v is False:
        return "false"
    if v is None:
        return "null"
    if isinstance(v, str):
        return quote(v, ascii_only)
    if isinstance(v, Obj):
        return "{" + ",".join(dump_json(k, ascii_only) + ":" + dump_json(x, ascii_only) for k, x in v) + "}"
    if isinstance(v, list):
        return "[" + ",".join(dump_json(x, ascii_only) for x in v) + "]"
    if isinstance(v, int):
        return str(v)
    raise TypeError(type(v))


def utf8(s):
    """s as UTF-8; a lone surrogate becomes its three-byte form, which is not valid UTF-8."""
    return s.encode("utf-8", "surrogatepass")


# 2^1024 - 2^970, halfway between the largest finite double and 2^1024.
HALFWAY = ("179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330"
           "286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069"
           "855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792")
NUMBERS = [
    "0", "-0", "1", "-1", "0.5", "-0.0", "1e999", "-1e400", "1e-400", "1E+308", "1e308", "1e309", "2e308",
    "1.7976931348623157e308", "1.7976931348623158e308", "1.7976931348623159e308", HALFWAY, "-" + HALFWAY,
    HALFWAY[:-1] + "1", HALFWAY + ".0", "0." + HALFWAY + "e309", "000" + HALFWAY, "9" * 309, "1" + "0" * 308,
    "9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809",
    "18446744073709551616", "9007199254740993", "4.9e-324", "2.4703282292062328e-324", "1e-325", "0e999999999999",
    "1e1000000000", "123456789012345678901234567890", "1.5e3", "01", "1.", ".5", "1e", "1e+", "+1", "0x10", "-",
    "1" + "0" * 400, "-62135596800", "1800003600", "4503599627370496", "-4503599627370496", "9223371974719179007",
    "9223371974719179008",
]
STRINGS = [
    "", "a", "x y", "completed", "failed", "incomplete", "cancelled", "COMPLETED", "completed\u0000", "realtime",
    "transcription", "invalid_request_error", "rate_limit_error", "insufficient_quota", "rate_limit_exceeded",
    "missing_scope", "session_expired", "invalid_api_key", "session_not_found", "authentication_error",
    "permission_error", "server_error", "AA==", "AAE=", "AAEC", "AA=A", "AAEC====", "A", "AB==", "AA\r\n==", "AA==AA==",
    "+/+/", "-_-_", "AAECAw==", "é", "ek_live_abcdefghijklmnop", "canary-secret-value",
    "eyJhbGciOiJub25lIn0.eyJ4IjoxfQ.c2ln", "\ud800", "\U0001f600", "e\u0301", "\u212a", "\u0378",
]
KEYS = [
    "type", "id", "status", "metadata", "delta", "response", "response_id", "item_id", "error", "code", "message",
    "param", "usage", "rate_limits", "remaining", "limit", "name", "reset_seconds", "session", "model", "exp",
    "access_token", "tokens", "auth_mode", "TYPE", "Type", "type ", "a", "é", "e\u0301", "\u212a",
    "K", "\u00c5", "A\u030a", "e" + "\u0301" * 30, "e" + "\u0301" * 31, "\u0378", "\ufdd0", "\ue000",
    "\U0001e5ee", "\U00030000", "\ud800", "",
]
EVENT_TYPES = [
    "session.created", "response.created", "response.output_audio.delta", "response.output_audio_transcript.delta",
    "response.done", "rate_limits.updated", "error", "input_audio_buffer.committed",
    "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed",
    "conversation.item.input_audio_transcription.failed", "session.updated", "response.output_item.added",
    "conversation.item.input_audio_transcription.segment", "future.event", "", "RESPONSE.DONE", "response.done ",
    "ek_live_abcdefghijklmnopqrstuv", "eyJhbGciOiJub25lIn0.eyJ4IjoxfQ.c2ln",
]
# Bytes an edit of a JSON document inserts: structure, escapes, literals, whitespace, invalid UTF-8.
JSON_UNITS = [
    b"{", b"}", b"[", b"]", b",", b":", b'"', b"\\", b"\\u0000", b"\\ud800", b"\\udc00", b"\\ud83d\\ude00",
    b"\\ud83d", b"\\udbff\\udfff", b"\\u00e9", b"\\u0065\\u0301", b"\\n", b"\\/", b"\\x", b"\\u12", b"\\U0041",
    b"\\'", b"true", b"false", b"null", b"nul", b"tru", b" ", b"\t", b"\r", b"\n", b"\xc2\xa0", b"\x0b", b"\x0c",
    b"\xff", b"\xc0\xaf", b"\xed\xa0\x80", b"\xf4\x90\x80\x80", b"\xe0\x80\x80", b"\xf0\x80\x80\x80", b"\xc2",
    b"\x80", b"\xef\xbb\xbf", b"\x00", b'"a":1,', b',"a":1',
]


def value_pool(draw, depth=0):
    """An interesting JSON value."""
    k = draw.below(10)
    if k < 3:
        return Raw(draw.choice(NUMBERS))
    if k < 6:
        return draw.choice(STRINGS) if draw.chance(0.7) else random_text(draw, most=4)
    if k == 6:
        return draw.choice([True, False, None])
    if k == 7 or depth > 2:
        return [] if draw.chance(0.5) else [value_pool(draw, depth + 1) for _ in range(draw.between(1, 3))]
    return Obj((draw.choice(KEYS), value_pool(draw, depth + 1)) for _ in range(draw.between(0, 3)))


def nodes(v):
    """Every container in v, the root included, and the deepest nesting; iterative, for documents 10,000 deep."""
    out, deepest, stack = [], 0, [(v, 1)]
    while stack:
        x, d = stack.pop()
        if isinstance(x, list):
            out.append(x)
            deepest = max(deepest, d)
            stack.extend((y, d + 1) for y in ((y for _, y in x) if isinstance(x, Obj) else x))
    return out, deepest


def metadata_map(draw):
    """A string map whose names test its rules: canonically equivalent pairs, runs of marks, Unicode 15.0."""
    return Obj((draw.choice(KEYS) if draw.chance(0.8) else random_text(draw, most=3),
                draw.choice(["v", "", None, Raw("1"), "é"]) if draw.chance(0.9) else value_pool(draw))
               for _ in range(draw.between(1, 4)))


def mutate_value(draw, v):
    """v changed at one node: a value replaced, a member removed, added, duplicated or renamed, the type changed, or
    a string map replaced."""
    containers, _ = nodes(v)
    if not containers:
        return value_pool(draw)
    c = draw.choice(containers)
    # Replacements dominate: a duplicate name or a lost type ends decoding before any typed field is read.
    action = draw.choice(["insert", "insert", "delete", "duplicate", "rename", "type", "map", "map",
                          "replace", "replace", "replace", "replace"])
    if action == "map":
        maps = [x for x in containers if isinstance(x, Obj) and any(k == "metadata" for k, _ in x)]
        if maps:
            m = draw.choice(maps)
            m[:] = [(k, metadata_map(draw) if k == "metadata" else x) for k, x in m]
            return v
        action = "replace"
    if isinstance(c, Obj):
        if action == "insert" or not c:
            c.insert(draw.below(len(c) + 1), (draw.choice(KEYS), value_pool(draw)))
        elif action == "delete":
            del c[draw.below(len(c))]
        elif action == "duplicate":
            c.append(c[draw.below(len(c))])
        elif action == "rename":
            i = draw.below(len(c))
            name = c[i][0]
            if not isinstance(name, Raw):  # an edited name, or the same name spelled with escapes
                name = edit(draw, name, ["ascii", "marks", "cases", "joiners"]) if draw.chance(0.5) else \
                    Raw('"' + "".join("\\u%04x" % u for u in units16(name)) + '"')
            c[i] = (name, c[i][1])
        elif action == "type" and c is v:
            v[:] = [(k, x) for k, x in v if k != "type"]
            v.insert(draw.below(len(v) + 1), ("type", draw.choice(EVENT_TYPES)))
        else:
            i = draw.below(len(c))
            c[i] = (c[i][0], value_pool(draw))
    else:
        if action == "insert" or not c:
            c.insert(draw.below(len(c) + 1), value_pool(draw))
        elif action == "delete":
            del c[draw.below(len(c))]
        else:
            c[draw.below(len(c))] = value_pool(draw)
    return v


def edit_bytes(draw, b):
    """b with one insertion, deletion or replacement, the unit drawn from JSON_UNITS or the text classes."""
    unit = draw.choice(JSON_UNITS) if draw.chance(0.7) else utf8(draw.choice(CLASSES[draw.choice(TEXT_CLASSES)]))
    action = draw.below(3) if b else 0
    if action == 0:
        i = draw.below(len(b) + 1)
        return b[:i] + unit + b[i:]
    i = draw.below(len(b))
    return b[:i] + b[i + 1:] if action == 1 else b[:i] + unit + b[i + 1:]


def nest(draw, b):
    """b inside arrays or objects to a depth around a bound: Codex's 64 or the wire's 10,000."""
    k = draw.choice([62, 63, 64, 65, 9998, 9999, 10000, 10001]) - draw.below(2)
    if draw.chance(0.5):
        return b"[" * k + b + b"]" * k
    return b'{"a":' * k + b + b"}" * k


# --- seeds ---

def load_fixtures(rel):
    path = FIXTURES / rel
    text = path.read_text(encoding="utf-8")
    if path.suffix == ".jsonl":
        return [json.loads(line) for line in text.splitlines() if line.strip()]
    return json.loads(text)


def b64url(b):
    return base64.urlsafe_b64encode(b).decode().rstrip("=")


JWT_HEADER = b64url(b'{"alg":"none","typ":"JWT"}')


def substitute(v):
    """A fixture value with every {"$jwt": claims} replaced by its token (spec/fixtures.md)."""
    if isinstance(v, dict):
        if len(v) == 1 and isinstance(v.get("$jwt"), dict):
            claims = json.dumps(v["$jwt"], separators=(",", ":"), sort_keys=True, ensure_ascii=False)
            return JWT_HEADER + "." + b64url(claims.encode()) + "." + b64url(b"sig")
        return {k: substitute(x) for k, x in v.items()}
    if isinstance(v, list):
        return [substitute(x) for x in v]
    return v


def strings_in(v):
    if isinstance(v, str):
        yield v
    elif isinstance(v, dict):
        for x in v.values():
            yield from strings_in(x)
    elif isinstance(v, list):
        for x in v:
            yield from strings_in(x)


def base_language(tag):
    return tag.lower().replace("_", "-").split("-")[0]


def seeds():
    """What the generators mutate, drawn from the fixtures. A said-guard side written as a repeat (spec/fixtures.md)
    is a bounds case, kilobytes to a megabyte long, which tests the caps rather than a reading: it seeds nothing."""
    said = [(c["script"], c["transcript"], base_language(c["language"])) for c in load_fixtures("said-guard.json")
            if isinstance(c["script"], str) and isinstance(c["transcript"], str)
            and base_language(c["language"]) in SAID_LANGUAGES and assigned15(c["script"] + c["transcript"])]
    texts = [t for _, t, _ in said] + [script for script, _, _ in said]
    runs = set()
    for text in texts:
        run = ""
        for ch in text + "\n":
            if ch in HAN_NUMERALS:
                run += ch
            elif run:
                runs.add(run)
                run = ""
    runs.update(["一亿一亿", "万万", "亿万", "零一百", "一百零二十", "一十零五", "一万零五千", "一五一十", "两十", "十十",
                 "一千万万", "一〇八", "二〇二四", "两三", "九" * 21, "一千万亿", "九千九百九十九万九千九百九十九亿九千九百九十九万九千九百九十九"])
    events = [c["raw"].encode() for c in load_fixtures("events/events.json")]
    events += [json.dumps(c["event"]).encode() for c in load_fixtures("ws/ws.json") if c["kind"] == "error-event"]
    docs = list(events)
    for c in load_fixtures("mint/mint.json"):
        for r in c["setup"].get("replies", []):
            if "json" in r:
                docs.append(json.dumps(r["json"]).encode())
            elif isinstance(r.get("body"), str):
                docs.append(r["body"].encode())
    tokens = []
    for c in load_fixtures("auth-file/auth-file.json"):
        f = c.get("setup", {}).get("file", {})
        if "json" in f:
            docs.append(json.dumps(substitute(f["json"]), ensure_ascii=False).encode())
        elif "raw" in f:
            docs.append(f["raw"].encode())
        elif "hex" in f:
            docs.append(bytes.fromhex(f["hex"]))
        tokens += [s for s in strings_in(substitute(f)) if s.count(".") == 2]
    for c in load_fixtures("app-server/app-server.jsonl"):
        docs.append(json.dumps(substitute(c["setup"].get("server", {}))).encode())
        tokens += [s for s in strings_in(substitute(c["setup"])) if s.count(".") == 2]
    for c in load_fixtures("private-sign-in/private-sign-in.json"):
        tokens += [s for s in strings_in(substitute(c["setup"])) if s.count(".") == 2]
    event_texts = []
    for e in events:
        try:
            v = parse_json(e.decode("utf-8"))
        except (ValueError, RecursionError):
            continue
        if isinstance(v, Obj) and nodes(v)[1] <= 100:
            event_texts.append(e.decode("utf-8"))
    return {"said": said, "texts": texts, "han": sorted(runs), "events": events, "event_texts": event_texts,
            "docs": docs, "tokens": sorted(set(tokens)), "pcm": load_fixtures("pcm16.json")}


# --- generators: each returns one input for its operation ---

def said_language(draw, base):
    if draw.chance(0.1):
        base = draw.choice(sorted(SAID_LANGUAGES))
    return draw.choice(SAID_LANGUAGES[base])


UNSPOKEN = set(".,;:!?\"()[]{}…–—/\\|_~·" + "‘’“”")

# Ways a transcript may differ from its script and still be read the same, or nearly: the guard decides which.
READINGS = [
    lambda s: s.replace("-", ""), lambda s: s.replace("-", " "), lambda s: s.replace(".", " "),
    lambda s: unicodedata.normalize("NFC", s), lambda s: unicodedata.normalize("NFD", s),
    lambda s: unicodedata.normalize("NFKC", s), lambda s: unicodedata.normalize("NFKD", s), str.lower, str.upper,
    lambda s: "".join(c for c in s if c not in UNSPOKEN),
    lambda s: "".join(str(unicodedata.decimal(c)) if unicodedata.decimal(c, None) is not None else c for c in s),
    lambda s: "".join(c for c in s if not unicodedata.category(c).startswith("M")),
]


def gen_said_matches(draw, s):
    script, transcript, base = draw.choice(s["said"])
    k = draw.below(10)
    if k >= 8:  # a script against a transformed reading of itself
        script = edit(draw, script) if draw.chance(0.7) else random_text(draw)
        transcript = draw.choice(READINGS)(script)
    elif k == 0:
        script = edit(draw, script)
    elif k == 1:
        transcript = edit(draw, transcript)
    elif k == 2:  # a script must match itself
        script = transcript = edit(draw, script)
    elif k == 3:
        script = transcript = edit(draw, transcript)
    elif k == 4:  # the same word added to both
        w = draw.choice(CLASSES[draw.choice(TEXT_CLASSES)])
        if draw.chance(0.5):
            script, transcript = script + " " + w, transcript + " " + w
        else:
            script, transcript = w + " " + script, w + " " + transcript
    elif k == 5:
        script = transcript = random_text(draw)
    elif k == 6:
        script, transcript = random_text(draw), random_text(draw)
    else:
        script, transcript = edit(draw, edit(draw, script)), edit(draw, transcript)
    return {"said": transcript, "spoken": script, "language": said_language(draw, base)}


def gen_said_tokens(draw, s):
    text = edit(draw, draw.choice(s["texts"])) if draw.chance(0.7) else random_text(draw)
    return {"text": text, "language": said_language(draw, draw.choice(sorted(SAID_LANGUAGES)))}


def gen_han(draw, s):
    k = draw.below(4)
    if k == 0:
        text = "".join(draw.choice(HAN_NUMERALS) for _ in range(draw.between(1, 14)))
    elif k == 1:
        text = draw.choice(s["han"]) + draw.choice(s["han"])
    else:
        text = draw.choice(s["han"])
        for _ in range(draw.between(1, 2)):
            text = edit(draw, text, ["han"] * 9 + ["digits", "marks", "joiners", "fullwidth"])
    return {"text": text}


def gen_echo(draw, s):
    return {"text": edit(draw, draw.choice(s["texts"])) if draw.chance(0.5) else random_text(draw)}


def gen_json_doc(draw, s, pool, by_byte=4):
    """A document for wire.valid, codex.valid and event.decode: a seed edited by byte (by_byte in 10) or by value,
    nested, or new."""
    doc = draw.choice(pool)
    k = draw.below(10)
    if k < by_byte:
        for _ in range(draw.between(1, 2)):
            doc = edit_bytes(draw, doc)
    elif k < 8:
        try:
            v = parse_json(doc.decode("utf-8"))
        except (ValueError, RecursionError):
            return edit_bytes(draw, doc)
        if nodes(v)[1] > 100:  # dump_json recurses; a deep seed is edited by byte
            return edit_bytes(draw, doc)
        for _ in range(draw.between(1, 3)):
            v = mutate_value(draw, v)
        doc = utf8(dump_json(v, ascii_only=draw.chance(0.3)))
    elif k == 8:
        doc = nest(draw, doc if draw.chance(0.5) else b"1")
    else:
        doc = utf8(dump_json(value_pool(draw)))
    return doc


def gen_wire(draw, s):
    return {"hex": gen_json_doc(draw, s, s["docs"]).hex()}


def gen_event(draw, s):
    """Mostly a parseable seed changed by value: an edited byte mostly makes invalid JSON, which never reaches a
    typed field. Some are new objects of a known type with members from the pools."""
    k = draw.below(20)
    if k < 2:
        return {"hex": gen_json_doc(draw, s, s["events"]).hex()}
    if k < 4:
        v = Obj([("type", draw.choice(EVENT_TYPES))] + [(draw.choice(KEYS), value_pool(draw)) for _ in range(draw.below(4))])
    else:
        v = parse_json(draw.choice(s["event_texts"]))
    for _ in range(draw.between(1, 3)):
        v = mutate_value(draw, v)
    return {"hex": utf8(dump_json(v, ascii_only=draw.chance(0.3))).hex()}


EXPS = ["0", "-0", "1", "-1", "1800003600", "9007199254740991", "9007199254740992", "9007199254740993",
        "4503599627370495", "4503599627370496", "-4503599627370495", "-4503599627370496", "4611686018427387904",
        "4611686018427392000", "9223371974719179007", "9223371974719179008", "9223372036854775807",
        "9223372036854775808", "-9223372036854775808", "-9223372036854775809", "-62135596800", "-62135596801",
        "1e3", "1.0", "1.5", "-0.0", "1E2", "01", "true", "null", '"5"', "[]", "{}", "1e999", "1" + "0" * 30]


def gen_jwt(draw, s):
    k = draw.below(5)
    if k == 0:
        return {"token": edit(draw, draw.choice(s["tokens"]), ["ascii", "ascii", "dashes", "spaces", "symbols", "letters"])}
    exp = draw.choice(EXPS)
    members = ['"exp":' + exp]
    for _ in range(draw.below(3)):
        members.insert(draw.below(len(members) + 1), draw.choice(
            ['"iat":1', '"EXP":5', '"exp":1', '"\\u0065xp":7', '"exp\\u0000":2', '"a":{"exp":3}', '"x":"\\ud800"',
             '"y":' + draw.choice(NUMBERS), '"z":"' + draw.choice(STRINGS).replace("\\", "") + '"']))
    payload = utf8("{" + ",".join(members) + "}")
    if k == 1:
        payload = edit_bytes(draw, payload)
    elif k == 2:  # nested to around Codex's bound, 64 levels with the payload's own
        d = draw.between(61, 64)
        payload = b'{"a":' + b"[" * d + b"]" * d + b"," + payload[1:]
    token = JWT_HEADER + "." + b64url(payload) + "." + b64url(b"sig")
    if k == 3:
        token = edit(draw, token, ["ascii", "dashes", "spaces", "symbols"])
    elif k == 4 and draw.chance(0.5):
        token = token.replace(".", "..", 1) if draw.chance(0.5) else token + "=" * draw.between(1, 3)
    return {"token": token}


# The account claim a private sign-in names (spec/credentials.md, AUTH-08): names spelt exactly, escaped and in
# another case, and values at and beyond each bound of the account ID.
AUTH_NAMES = ['"https://api.openai.com/auth"', '"https:\\/\\/api.openai.com\\/auth"', '"https://api.openai.com/AUTH"',
              '"https://api.openai.com/auth "', '"https://api.openai.com/\\u0061uth"']
ACCOUNT_NAMES = ['"chatgpt_account_id"', '"chatgpt_account_id"', '"chatgpt_account_ID"', '"chatgpt\\u005faccount_id"',
                 '"account_id"']
ACCOUNTS = ['"acct-0123"', '"acct-0123"', '""', '"a"', '"' + "a" * 256 + '"', '"' + "a" * 257 + '"', '"acct 1"',
            '"acct.1_x-Y"', '"\\u0061cct"', '"acct\\u0000"', '"acct-\\u00e9"', '"acct-\u00e9"', '"acct\\"1"',
            '"\\ud800"', '"acct/1"', '"acct=1"', '"acct\\n"', "7", "null", "true", "[]", "{}"]


def gen_jwt_account(draw, s):
    k = draw.below(5)
    if k == 0:
        return {"token": edit(draw, draw.choice(s["tokens"]), ["ascii", "dashes", "spaces", "symbols", "letters"])}
    inner = [draw.choice(ACCOUNT_NAMES) + ":" + draw.choice(ACCOUNTS)]
    for _ in range(draw.below(3)):
        inner.insert(draw.below(len(inner) + 1),
                     draw.choice(['"x":1', '"chatgpt_plan_type":"pro"', '"chatgpt_account_id":"dup"']))
    value = "{" + ",".join(inner) + "}" if draw.chance(0.9) else draw.choice(ACCOUNTS)
    claim = draw.choice(AUTH_NAMES) + ":" + value
    members = ['"exp":1800003600', claim]
    for _ in range(draw.below(3)):
        members.insert(draw.below(len(members) + 1),
                       draw.choice(['"iat":1', '"https://api.openai.com/profile":{"email":"a@b"}', claim]))
    payload = utf8("{" + ",".join(members) + "}")
    if k == 1:
        payload = edit_bytes(draw, payload)
    token = JWT_HEADER + "." + b64url(payload) + "." + b64url(b"sig")
    if k == 2:
        token = edit(draw, token, ["ascii", "dashes", "spaces", "symbols"])
    elif k == 3:
        token = draw.choice([token + "=", token + ".", token + ".c2ln", token[:-4], token.replace(".", "..", 1)])
    return {"token": token}


FLOAT_BITS = [0x00000000, 0x80000000, 0x3F800000, 0xBF800000, 0x3F800001, 0xBF800001, 0x3F7FFFFF, 0xBF7FFFFF,
              0x7FC00000, 0xFFC00000, 0x7F800001, 0x7FBFFFFF, 0x7F800000, 0xFF800000, 0x00000001, 0x80000001,
              0x007FFFFF, 0x3F000000, 0x38000100, 0x47000000, 0x7F7FFFFF, 0xFF7FFFFF]


def pcm_bytes(draw):
    k = draw.below(3)
    n = draw.choice([0, 1, 2, 3, 4, 5, 6, 8, 10, 33])
    if k == 0:
        return bytes(draw.below(256) for _ in range(n))
    sample = draw.choice([0, 1, 327, 328, 329, -327, -328, -329, 32767, -32768])
    return int.to_bytes(sample & 0xFFFF, 2, "little") * draw.between(0, 4000)


def gen_pcm_encode(draw, s):
    bits = []
    for _ in range(draw.between(0, 8)):
        if draw.chance(0.5):
            bits.append(draw.choice(FLOAT_BITS))
        elif draw.chance(0.5):  # k/32767 and its neighbours: the truncation boundaries
            v = int.from_bytes(struct.pack(">f", draw.between(-32767, 32767) / 32767), "big")
            bits.append((v + draw.between(-1, 1)) & 0xFFFFFFFF)
        else:
            bits.append(draw.below(1 << 16) << 16 | draw.below(1 << 16))
    return {"bits": bits}


def gen_pcm_decode(draw, s):
    return {"hex": pcm_bytes(draw).hex()}


def wav_header(n, rate):
    le = lambda v, w: (v & ((1 << (8 * w)) - 1)).to_bytes(w, "little")
    return (b"RIFF" + le(36 + n, 4) + b"WAVEfmt " + le(16, 4) + le(1, 2) + le(1, 2) + le(rate, 4) + le(rate * 2, 4)
            + le(2, 2) + le(16, 2) + b"data" + le(n, 4))


def gen_pcm_wav(draw, s):
    rate = draw.choice([0, 1, -1, 8000, 24000, 48000, 2147483647, 2147483648, 4294967295, 4294967296, -2147483648])
    return {"hex": pcm_bytes(draw)[:64].hex(), "rate": rate}


def gen_pcm_valid_wav(draw, s):
    seeds_ = [bytes.fromhex(c["hex"]) for c in s["pcm"] if c["op"] == "validWAV"]
    pcm = pcm_bytes(draw)[:40]
    wav = draw.choice(seeds_) if draw.chance(0.3) else wav_header(len(pcm), draw.choice([24000, 24000, 16000])) + pcm
    k = draw.below(4)
    if k == 0 and wav:
        i = draw.below(len(wav))
        wav = wav[:i] + bytes([draw.below(256)]) + wav[i + 1:]
    elif k == 1:
        wav = wav[:draw.below(len(wav) + 1)]
    elif k == 2:
        wav = wav + bytes(draw.below(256) for _ in range(draw.between(1, 3)))
    n = max(len(wav) - 44, 0)
    cap = draw.choice([0, -1, n, n - 1, n + 1, 1 << 20, 4294967259, 9223372036854775807])
    return {"hex": wav.hex(), "max": cap}


def gen_pcm_plausible(draw, s):
    script = random_text(draw, ["ascii", "letters", "unspaced", "marks", "digits", "cases", "jamo"], most=40)
    return {"hex": pcm_bytes(draw).hex(), "script": script * draw.between(1, 3)}


GENERATORS = {
    "said.matches": gen_said_matches, "said.tokens": gen_said_tokens, "han.parse": gen_han, "jwt.exp": gen_jwt,
    "jwt.account": gen_jwt_account, "echo": gen_echo, "wire.valid": gen_wire, "codex.valid": gen_wire, "event.decode": gen_event,
    "pcm.encode": gen_pcm_encode, "pcm.decode": gen_pcm_decode, "pcm.wav": gen_pcm_wav,
    "pcm.validWAV": gen_pcm_valid_wav, "pcm.plausible": gen_pcm_plausible,
}


def generate(seed, count, ops=tuple(OPS)):
    """count cases per operation, each {"id", "op", "input"}; the same seed always gives the same cases."""
    s = seeds()
    cases = []
    for op in ops:
        draw = Draw(f"{seed}:{op}")
        for i in range(count):
            cases.append({"id": f"{op}-{i}", "op": op, "input": GENERATORS[op](draw, s)})
    return cases


# --- the documented differences ---

def said_text(c):
    i = c["input"]
    return i.get("said", "") + i.get("spoken", "") + i.get("text", "")


# Each entry: the spec sentence that documents a difference, and when a disagreement is that difference. The
# said-guard generators keep to en and zh and to Unicode 15.0, so neither applies to a generated case; a case one
# absorbed would be reported.
ALLOWED = [
    ("spell-out", "spec/parity.md: \"Swift must declare `spellout:<lang>` for `ar de es fr hi it ja ko pt` ...; Go "
     "declares none\"; spec/said-guard.md: \"the locale's spell-out where the implementation has one (Swift: ICU), "
     "else none\"",
     lambda c, go, swift: c["op"] == "said.matches" and base_language(c["input"]["language"]) in SPELLOUT
     and go is False and swift is True),
    ("unicode-tables", "spec/said-guard.md: \"a character newer than the platform's Unicode tables (a Unicode 16 "
     "digit on macOS 14 reads as a symbol in Swift; the same table difference can only refuse, since an unknown "
     "character is a token that matches only itself)\"",
     lambda c, go, swift: c["op"] in ("said.matches", "said.tokens") and not assigned15(said_text(c))),
]


def canonical(v):
    """The JSON text of v with sorted keys: true and 1 differ, as they must."""
    return json.dumps(v, sort_keys=True, ensure_ascii=False)


def compare(cases, go, swift):
    """The cases whose outputs differ, and how many each allow-list entry absorbed. go and swift map id to output."""
    diverged, allowed = [], {name: 0 for name, _, _ in ALLOWED}
    for c in cases:
        g, w = go[c["id"]], swift[c["id"]]
        if canonical(g) == canonical(w):
            continue
        entry = next((name for name, _, applies in ALLOWED if applies(c, g, w)), None)
        if entry:
            allowed[entry] += 1
        else:
            diverged.append((c, g, w))
    return diverged, allowed


def shortest(diverged):
    """The shortest diverging case of each operation, by the length of its input's JSON."""
    best = {}
    for c, g, w in diverged:
        if c["op"] not in best or len(canonical(c["input"])) < len(canonical(best[c["op"]][0]["input"])):
            best[c["op"]] = (c, g, w)
    return best


def candidate(c, go, swift):
    """A fixture skeleton for a diverging case; expect is left for the spec to decide."""
    op, i = c["op"], c["input"]
    note = f"differential {c['id']}: go {canonical(go)}, swift {canonical(swift)}"
    if op == "said.matches":
        return {"component": "said-guard", "script": i["spoken"], "transcript": i["said"], "language": i["language"],
                "expect": None, "note": note}
    if op in ("said.tokens", "han.parse", "echo"):
        return {"component": "said-guard", "script": i["text"], "transcript": i["text"],
                "language": i.get("language", "zh"), "expect": None, "note": note}
    if op in ("event.decode", "wire.valid"):
        raw = bytes.fromhex(i["hex"])
        try:
            return {"component": "session", "kind": "event", "raw": raw.decode("utf-8"), "expect": None, "note": note}
        except UnicodeDecodeError:
            return {"component": "session", "kind": "event", "hex": i["hex"], "expect": None, "note": note}
    if op == "codex.valid":
        return {"component": "credentials", "setup": {"file": {"kind": "regular", "hex": i["hex"]}}, "expect": None,
                "note": note}
    if op == "jwt.exp":
        return {"component": "credentials", "setup": {"file": {"kind": "regular", "json": {"tokens": {
            "access_token": i["token"]}}}}, "expect": None, "note": note}
    if op == "jwt.account":
        return {"component": "credentials", "setup": {"privateSignIn": {"token": i["token"], "folder": "private"}},
                "expect": None, "note": note}
    return {"component": "pcm16", "op": op.split(".", 1)[1], **i, "expect": None, "note": note}


# --- running the oracles ---

class HarnessError(Exception):
    pass


def write_cases(path, cases):
    with open(path, "w", encoding="ascii") as f:
        for c in cases:
            f.write(json.dumps(c, ensure_ascii=True, separators=(",", ":")) + "\n")


def read_outputs(path, cases, name):
    """id → output for cases, from an oracle's output file. A missing case is a crash at that case."""
    want = {c["id"]: c["op"] for c in cases}
    got = {}
    if path.exists():
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError as e:
            raise HarnessError(f"{name}: {path} is not UTF-8 ({e})") from None
        # "\n" alone ends a line: splitlines would also split at U+0085 or U+2028, which a JSON string may hold raw.
        for n, line in enumerate(text.split("\n"), 1):
            if not line:
                continue
            try:
                r = json.loads(line)
            except ValueError as e:
                raise HarnessError(f"{name}: line {n} of {path} is not JSON ({e}): {line[:200]}") from None
            if not isinstance(r, dict) or "output" not in r:
                raise HarnessError(f"{name}: line {n} of {path} is no answer object with an output: {line[:200]}")
            if r.get("id") not in want or r["id"] in got or r.get("op") != want[r["id"]]:
                raise HarnessError(f"{name}: unexpected answer {line[:200]}")
            got[r["id"]] = r["output"]
    missing = [c for c in cases if c["id"] not in got]
    if missing:
        raise HarnessError(f"{name}: no answer from case {missing[0]['id']} on ({len(missing)} missing); its input: "
                           f"{canonical(missing[0]['input'])[:2000]}")
    return got


def tail(path, lines=40):
    return "\n".join(path.read_text(encoding="utf-8", errors="replace").splitlines()[-lines:]) if path.exists() else ""


def kill_group(proc):
    """Ends proc and everything it started in its own session, and reaps proc."""
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass  # it ended on its own meanwhile
    proc.wait()


def finish(proc, deadline, name):
    """proc's exit status; at the deadline (time.monotonic()) its session is killed and the run is a harness failure."""
    try:
        return proc.wait(timeout=max(0, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        raise HarnessError(f"{name} ran past its --timeout deadline") from None
    finally:
        if proc.poll() is None:
            kill_group(proc)


def run_oracles(work, cases, timeout=DEFAULT_TIMEOUT):
    """Both implementations' outputs, id → output each. Swift runs alongside the Go packages; each oracle command
    has timeout seconds from its start."""
    for tool in ("go", "swift"):
        if shutil.which(tool) is None:
            raise HarnessError(f"{tool} is not on PATH")
    swift_in, swift_out, swift_log = work / "swift-in.jsonl", work / "swift-out.jsonl", work / "swift.log"
    write_cases(swift_in, cases)
    swift_out.unlink(missing_ok=True)  # a kept folder's last answers must not stand in for this run's
    env = dict(os.environ, SAYSO_ORACLE_IN=str(swift_in), SAYSO_ORACLE_OUT=str(swift_out))
    env.pop("SAYSO_RESULTS", None)  # an oracle run is no fixture run
    with open(swift_log, "w") as log:
        swift = subprocess.Popen(["swift", "test", "--filter", "OracleTests/testOracle"], cwd=ROOT, env=env,
                                 stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    swift_deadline = time.monotonic() + timeout
    go = {}
    failures = []
    try:
        for pkg in sorted(set(OPS[c["op"]] for c in cases)):
            mine = [c for c in cases if OPS[c["op"]] == pkg]
            gin, gout, glog = work / f"go-{pkg}-in.jsonl", work / f"go-{pkg}-out.jsonl", work / f"go-{pkg}.log"
            write_cases(gin, mine)
            gout.unlink(missing_ok=True)
            genv = dict(env, SAYSO_ORACLE_IN=str(gin), SAYSO_ORACLE_OUT=str(gout))
            with open(glog, "w") as log:
                proc = subprocess.Popen(["go", "test", "-count=1", "-run", "^TestOracle$", "./" + pkg],
                                        cwd=ROOT / "go", env=genv, stdout=log, stderr=subprocess.STDOUT,
                                        start_new_session=True)
            try:
                code = finish(proc, time.monotonic() + timeout, f"go ./{pkg}")
                go.update(read_outputs(gout, mine, f"go ./{pkg}"))
            except HarnessError as e:
                failures.append(f"{e}\n{tail(glog)}")
                continue
            if code != 0:
                failures.append(f"go ./{pkg} exited {code}\n{tail(glog)}")
        try:
            code = finish(swift, swift_deadline, "swift test")
        except HarnessError as e:
            failures.append(f"{e}\n{tail(swift_log)}")
            raise HarnessError("\n\n".join(failures)) from None
    finally:
        if swift.poll() is None:
            kill_group(swift)
    try:
        swift_got = read_outputs(swift_out, cases, "swift")
        if code != 0:
            failures.append(f"swift test exited {code}\n{tail(swift_log)}")
    except HarnessError as e:
        failures.append(f"{e}\n{tail(swift_log)}")
    if failures:
        raise HarnessError("\n\n".join(failures))
    return go, swift_got


def report(cases, diverged, allowed, elapsed):
    by_op = {}
    for c, _, _ in diverged:
        by_op[c["op"]] = by_op.get(c["op"], 0) + 1
    ops = sorted({c["op"] for c in cases})
    for op in ops:
        n = sum(1 for c in cases if c["op"] == op)
        print(f"differential: {op}: {n} cases, {by_op.get(op, 0)} diverged")
    for name, cite, _ in ALLOWED:
        print(f"differential: allowed {name}: {allowed[name]} cases ({cite})")
    for op, (c, g, w) in sorted(shortest(diverged).items()):
        print(f"\ndifferential: {op}: shortest of {by_op[op]} diverging inputs, {c['id']}:")
        print(f"  input: {canonical(c['input'])}")
        print(f"  go:    {canonical(g)}")
        print(f"  swift: {canonical(w)}")
        print("  candidate fixture: " + json.dumps(candidate(c, g, w), ensure_ascii=False))
    print(f"\ndifferential: {len(cases)} cases, {len(diverged)} diverged, {elapsed:.0f} s")


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--seed", type=int, default=DEFAULT_SEED)
    p.add_argument("--count", type=int, default=DEFAULT_COUNT, help="cases per operation")
    p.add_argument("--ops", default=",".join(OPS), help="comma-separated operations")
    p.add_argument("--keep", help="keep the cases, outputs and logs in this folder")
    p.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT,
                   help="seconds each oracle command may run, its build included")
    a = p.parse_args(argv)
    ops = list(dict.fromkeys(o for o in a.ops.split(",") if o))  # a repeated operation runs once
    unknown = [o for o in ops if o not in OPS]
    if unknown or not ops or a.count < 1 or not a.timeout > 0:
        p.error(f"--count and --timeout must be positive and --ops a list of {', '.join(OPS)}")
    # The cases depend on the seed, the fixtures and, through READINGS, Python's Unicode tables.
    print(f"differential: seed {a.seed}, {a.count} cases per operation, python {sys.version.split()[0]} "
          f"(Unicode {unicodedata.unidata_version})", flush=True)
    start = time.monotonic()
    cases = generate(a.seed, a.count, ops)
    # Absolute, because Go runs in ROOT/go and Swift in ROOT, not in the caller's folder.
    work = pathlib.Path(a.keep).resolve() if a.keep else pathlib.Path(tempfile.mkdtemp(prefix="sayso-differential-"))
    work.mkdir(parents=True, exist_ok=True)
    try:
        go, swift = run_oracles(work, cases, a.timeout)
    except HarnessError as e:
        print(f"differential: harness failure: {e}", file=sys.stderr)
        print(f"differential: files kept in {work}", file=sys.stderr)
        return 2
    diverged, allowed = compare(cases, go, swift)
    report(cases, diverged, allowed, time.monotonic() - start)
    if not a.keep:
        shutil.rmtree(work, ignore_errors=True)
    return 1 if diverged else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
