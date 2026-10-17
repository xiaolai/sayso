"""Tests for tools/differential.py's pure parts, and for its runner against stand-in oracles: a harness that cannot
fail is no harness, so each way an oracle could fall silent must end in exit 2, and a disagreement in exit 1."""
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unicodedata
import unittest
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parent.parent
_spec = importlib.util.spec_from_file_location("differential", ROOT / "tools" / "differential.py")
d = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(d)

TEXT_OPS = ("said.matches", "said.tokens", "han.parse", "echo")


def texts(c):
    """The strings a case hands to an implementation as text."""
    return [v for v in c["input"].values() if isinstance(v, str) and c["op"] in TEXT_OPS + ("jwt.exp", "jwt.account", "pcm.plausible")]


class Generation(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cases = d.generate(7, 60)

    def test_same_seed_same_cases(self):
        self.assertEqual(json.dumps(d.generate(7, 60)), json.dumps(self.cases))

    def test_other_seed_other_cases(self):
        self.assertNotEqual(json.dumps(d.generate(8, 60)), json.dumps(self.cases))

    def test_count_per_operation_and_unique_ids(self):
        self.assertEqual(len(self.cases), 60 * len(d.OPS))
        self.assertEqual(len({c["id"] for c in self.cases}), len(self.cases))
        for op in d.OPS:
            self.assertEqual(sum(1 for c in self.cases if c["op"] == op), 60, op)

    def test_cases_travel_as_ascii_json(self):
        with tempfile.TemporaryDirectory() as t:
            p = pathlib.Path(t, "in.jsonl")
            d.write_cases(p, self.cases)
            lines = p.read_bytes().split(b"\n")
        self.assertEqual(lines[-1], b"")
        self.assertEqual(len(lines) - 1, len(self.cases))
        self.assertTrue(all(b < 0x80 for line in lines for b in line))
        self.assertEqual([json.loads(line) for line in lines[:-1]], self.cases)

    def test_text_has_no_lone_surrogate(self):
        # A Swift String cannot hold one, so a text input with one would not reach both implementations as written.
        for c in self.cases:
            for s in texts(c):
                self.assertFalse(any(0xD800 <= ord(ch) <= 0xDFFF for ch in s), c["id"])

    def test_said_guard_inputs_are_unicode_15(self):
        for c in self.cases:
            if c["op"].startswith("said."):
                self.assertTrue(d.assigned15(d.said_text(c)), c["id"])

    def test_said_guard_languages_are_en_and_zh(self):
        for c in self.cases:
            if c["op"].startswith("said."):
                self.assertIn(d.base_language(c["input"]["language"]), ("en", "zh"), c["id"])

    def test_every_class_unit_is_unicode_15(self):
        for name, units in d.CLASSES.items():
            for u in units:
                self.assertTrue(d.assigned15(u), f"{name}: {u!r}")

    def test_digits_cover_every_unicode_15_block(self):
        # As far as Python's own tables reach: every decimal digit assigned in 15.0 counts from a listed zero.
        for cp in range(0x110000):
            ch = chr(cp)
            if unicodedata.category(ch) == "Nd" and d.assigned15(ch):
                self.assertIn(cp - unicodedata.decimal(ch), d.ND_ZEROS, hex(cp))
        self.assertTrue(all(d.assigned15(chr(z + 9)) for z in d.ND_ZEROS))

    def test_draw_uses_random_alone(self):
        a, b = d.Draw("x"), d.Draw("x")
        self.assertEqual([a.below(1000) for _ in range(50)], [b.below(1000) for _ in range(50)])
        self.assertTrue(all(0 <= d.Draw(i).below(3) < 3 for i in range(100)))


class UnicodeTable(unittest.TestCase):
    def test_known_points(self):
        self.assertTrue(d.assigned15("a" + chr(0x1E4F0)))  # Nag Mundari, new in 15.0
        # Private use and surrogates are in x/text's set; lone surrogates are kept out of text by the generators.
        self.assertTrue(d.assigned15(chr(0xE000) + chr(0x10FFFD) + chr(0xD800)))
        for c in (0x0378, 0xFFFF, 0xFDD0, 0x1E5F1, 0x10FFFF):  # unassigned, noncharacters, Unicode 16
            self.assertFalse(d.assigned15(chr(c)), hex(c))
        self.assertTrue(d.assigned15(""))


class Json(unittest.TestCase):
    def test_round_trip_keeps_duplicates_and_numbers_as_written(self):
        text = '{"a":1,"a":1e999,"b":[-0.0,"x",true,null],"c":{}}'
        self.assertEqual(d.dump_json(d.parse_json(text)), text)

    def test_lone_surrogate_is_escaped_and_its_bytes_are_not_utf8(self):
        s = "a" + chr(0xD800)
        self.assertEqual(d.quote(s), '"a' + chr(92) + 'ud800"')
        self.assertEqual(d.utf8(s), b"a\xed\xa0\x80")

    def test_ascii_only_spells_astral_characters_as_pairs(self):
        self.assertEqual(d.quote(chr(0x1F600), ascii_only=True), '"' + chr(92) + "ud83d" + chr(92) + 'ude00"')

    def test_nodes_reaches_ten_thousand_levels(self):
        deep = d.parse_json("[" * 50 + "1" + "]" * 50)
        self.assertEqual(d.nodes(deep)[1], 50)
        v = []
        for _ in range(10000):
            v = [v]
        self.assertEqual(d.nodes(v)[1], 10001)


def case(op, **inp):
    return {"id": f"{op}-0", "op": op, "input": inp}


class Comparison(unittest.TestCase):
    def test_true_and_one_differ(self):
        c = case("wire.valid", hex="")
        self.assertEqual(len(d.compare([c], {c["id"]: True}, {c["id"]: 1})[0]), 1)

    def test_key_order_does_not(self):
        c = case("event.decode", hex="")
        self.assertEqual(d.compare([c], {c["id"]: {"a": 1, "b": 2}}, {c["id"]: {"b": 2, "a": 1}})[0], [])

    def test_spell_out_absorbs_only_a_swift_acceptance_in_its_languages(self):
        fr = case("said.matches", said="vingt", spoken="20", language="fr-CA")
        en = case("said.matches", said="twenty", spoken="20", language="en")
        diverged, allowed = d.compare([fr], {fr["id"]: False}, {fr["id"]: True})
        self.assertEqual((diverged, allowed["spell-out"]), ([], 1))
        self.assertEqual(len(d.compare([fr], {fr["id"]: True}, {fr["id"]: False})[0]), 1)
        self.assertEqual(len(d.compare([en], {en["id"]: False}, {en["id"]: True})[0]), 1)

    def test_unicode_tables_absorb_only_said_guard_text_beyond_15(self):
        new = case("said.matches", said=chr(0x1E5F1), spoken=chr(0x1E5F1), language="en")
        old = case("said.matches", said=chr(0x1E4F1), spoken=chr(0x1E4F1), language="en")
        event = case("event.decode", hex=chr(0x1E5F1).encode().hex())
        diverged, allowed = d.compare([new, old, event], {new["id"]: True, old["id"]: True, event["id"]: 1},
                                      {new["id"]: False, old["id"]: False, event["id"]: 2})
        self.assertEqual(allowed["unicode-tables"], 1)
        self.assertEqual([c["id"] for c, _, _ in diverged], [old["id"], event["id"]])

    def test_every_allowance_cites_the_spec(self):
        for name, cite, _ in d.ALLOWED:
            self.assertRegex(cite, r"^spec/[a-z-]+\.md: \"", name)

    def test_shortest_per_operation(self):
        a = dict(case("echo", text="long text"), id="echo-1")
        b = dict(case("echo", text="ab"), id="echo-2")
        c = case("han.parse", text="x")
        best = d.shortest([(a, 1, 2), (b, 1, 2), (c, 1, 2)])
        self.assertEqual((best["echo"][0]["id"], best["han.parse"][0]["id"]), ("echo-2", "han.parse-0"))

    def test_said_candidate_puts_the_script_in_script(self):
        f = d.candidate(case("said.matches", said="heard", spoken="written", language="en"), True, False)
        self.assertEqual((f["script"], f["transcript"], f["expect"]), ("written", "heard", None))


class Outputs(unittest.TestCase):
    def setUp(self):
        self.t = tempfile.TemporaryDirectory()
        self.path = pathlib.Path(self.t.name, "out.jsonl")
        self.cases = [case("echo", text="a"), dict(case("echo", text="b"), id="echo-1")]

    def tearDown(self):
        self.t.cleanup()

    def write(self, *rows):
        self.path.write_text("".join(json.dumps(r, ensure_ascii=False) + "\n" for r in rows), encoding="utf-8")

    def test_every_answer_read(self):
        self.write({"id": "echo-0", "op": "echo", "output": "x" + chr(0x2028) + chr(0x85)},
                   {"id": "echo-1", "op": "echo", "output": None})
        self.assertEqual(d.read_outputs(self.path, self.cases, "t"),
                         {"echo-0": "x" + chr(0x2028) + chr(0x85), "echo-1": None})

    def test_a_missing_answer_names_its_case(self):
        self.write({"id": "echo-0", "op": "echo", "output": "x"})
        with self.assertRaisesRegex(d.HarnessError, "no answer from case echo-1"):
            d.read_outputs(self.path, self.cases, "t")

    def test_no_file_is_no_answer(self):
        with self.assertRaisesRegex(d.HarnessError, "no answer from case echo-0"):
            d.read_outputs(self.path, self.cases, "t")

    def test_a_repeated_or_foreign_answer_fails(self):
        for rows in ([{"id": "echo-0", "op": "echo", "output": 1}] * 2,
                     [{"id": "echo-0", "op": "wire.valid", "output": 1}],
                     [{"id": "other", "op": "echo", "output": 1}]):
            self.write(*rows)
            with self.assertRaisesRegex(d.HarnessError, "unexpected answer"):
                d.read_outputs(self.path, self.cases, "t")


class Wiring(unittest.TestCase):
    def test_every_operation_is_routed_generated_and_answered(self):
        swift = (ROOT / "Tests" / "ConformanceTests" / "OracleTests.swift").read_text(encoding="utf-8")
        self.assertEqual(set(d.GENERATORS), set(d.OPS))
        for op, pkg in d.OPS.items():
            go = (ROOT / "go" / pkg / "oracle_test.go").read_text(encoding="utf-8")
            self.assertIn(f'"{op}"', go, op)
            self.assertIn(f'case "{op}":', swift, op)


# A stand-in for the go and swift commands: it answers every case of SAYSO_ORACLE_IN with the case's input, or, by
# FAKE_<TOOL>, answers nothing, stops after the first case, exits 1 after answering, or answers one case differently.
# It refuses to run with SAYSO_RESULTS set, which would make an oracle run overwrite a suite's results.
FAKE = """import json, os, sys
if "SAYSO_RESULTS" in os.environ:
    sys.exit(3)
mode = os.environ.get("FAKE_" + os.path.basename(sys.argv[0]).upper(), "ok")
lines = [l for l in open(os.environ["SAYSO_ORACLE_IN"], encoding="ascii").read().split("\\n") if l]
with open(os.environ["SAYSO_ORACLE_OUT"], "w", encoding="utf-8") as out:
    for i, line in enumerate(lines):
        c = json.loads(line)
        if mode == "silent" or (mode == "crash" and i == 1):
            break
        v = "other" if mode == "differ" and i == 0 else c["input"]
        out.write(json.dumps({"id": c["id"], "op": c["op"], "output": v}) + "\\n")
sys.exit(1 if mode in ("crash", "fail") else 0)
"""


class Runner(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bin = tempfile.TemporaryDirectory()
        for tool in ("go", "swift"):
            p = pathlib.Path(cls.bin.name, tool)
            p.write_text(f"#!{sys.executable} -I\n" + FAKE, encoding="utf-8")
            p.chmod(0o755)

    @classmethod
    def tearDownClass(cls):
        cls.bin.cleanup()

    def run_tool(self, *args, **modes):
        env = dict(os.environ, PATH=self.bin.name + os.pathsep + os.environ.get("PATH", ""),
                   **{f"FAKE_{k.upper()}": v for k, v in modes.items()})
        return subprocess.run([sys.executable, "-I", str(ROOT / "tools" / "differential.py"), "--count", "3",
                               "--ops", "echo,said.matches,jwt.exp", *args], env=env, capture_output=True, text=True)

    def test_agreement_passes_and_prints_the_seed(self):
        r = self.run_tool("--seed", "42")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("differential: seed 42,", r.stdout)
        self.assertIn("9 cases, 0 diverged", r.stdout)

    def test_a_disagreement_fails_with_its_input(self):
        r = self.run_tool(swift="differ")
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("shortest of 1 diverging inputs", r.stdout)
        self.assertIn("candidate fixture", r.stdout)

    def test_a_silent_or_failing_oracle_is_a_harness_failure(self):
        for modes in ({"swift": "silent"}, {"go": "silent"}, {"swift": "crash"}, {"go": "crash"}, {"go": "fail"},
                      {"swift": "fail"}):
            r = self.run_tool(**modes)
            self.assertEqual(r.returncode, 2, f"{modes}: {r.stdout}{r.stderr}")
            self.assertIn("harness failure", r.stderr, modes)

    def test_oracles_get_their_files_and_no_results_path(self):
        cases = [case("echo", text="a")]
        with tempfile.TemporaryDirectory() as t, mock.patch.dict(os.environ, {
                "PATH": self.bin.name + os.pathsep + os.environ.get("PATH", ""), "SAYSO_RESULTS": "/nonexistent"}):
            go, swift = d.run_oracles(pathlib.Path(t), cases)
            self.assertTrue(pathlib.Path(t, "swift-in.jsonl").exists())
            self.assertTrue(pathlib.Path(t, "go-conformance-in.jsonl").exists())
        self.assertEqual((go, swift), ({"echo-0": {"text": "a"}}, {"echo-0": {"text": "a"}}))

    def test_bad_arguments_are_refused_before_any_run(self):
        for args in (["--count", "0"], ["--ops", "nope"], ["--ops", ""]):
            r = subprocess.run([sys.executable, "-I", str(ROOT / "tools" / "differential.py"), *args],
                               capture_output=True, text=True, env=dict(os.environ, PATH=""))
            self.assertEqual(r.returncode, 2, args)
            self.assertNotIn("harness failure", r.stderr, args)


if __name__ == "__main__":
    unittest.main()
