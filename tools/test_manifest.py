"""Tests for tools/manifest.py's schema: a member spec/fixtures.md does not define is refused wherever it appears."""
import copy
import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("manifest", ROOT / "tools" / "manifest.py")
manifest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(manifest)

MINT = {"id": "m", "component": "mint", "note": "n",
        "setup": {"origin": "test", "replies": [{"status": 200, "json": {}}]},
        "expect": {"class": None, "request": {"method": "POST"}}}
SESSION = {"id": "s", "component": "session", "kind": "close", "code": 1006, "reason": "", "expect": {"class": "network"}}
PCM = {"id": "p", "component": "pcm16", "op": "plausible", "pcm": {"samples": 1, "value": 0}, "script": "", "expect": False}
CREDENTIALS = {"id": "c", "component": "credentials",
               "setup": {"file": {"kind": "regular", "rawRepeat": {"text": "a", "count": 1}},
                         "client": {"calls": [{"method": "x"}]}},
               "steps": [{"op": "token", "expect": {"state": "ready"}}]}


def with_member(case, path, key="bogus"):
    """A copy of case with key added to the first object at path."""
    c = copy.deepcopy(case)
    manifest.objects(c, path)[0][key] = 1
    return c


class Schema(unittest.TestCase):
    def test_the_corpus_passes(self):
        for path in sorted(manifest.FIXTURES.rglob("*")):
            if path.suffix in (".json", ".jsonl") and path.name not in manifest.SKIP:
                for case in manifest.load(path):
                    self.assertEqual(manifest.unknown_members(case), [], case["id"])

    def test_known_cases_pass(self):
        for case in (MINT, SESSION, PCM, CREDENTIALS):
            self.assertEqual(manifest.unknown_members(case), [], case["id"])

    def test_an_unknown_member_is_named_at_every_level(self):
        for case, paths in [
            (MINT, ["", "setup", "setup.replies[]", "expect", "expect.request"]),
            (SESSION, ["", "expect"]),
            (PCM, ["", "pcm"]),
            (CREDENTIALS, ["", "setup", "setup.file", "setup.file.rawRepeat", "setup.client", "setup.client.calls[]",
                           "steps[]", "steps[].expect"]),
        ]:
            for path in paths:
                self.assertEqual(manifest.unknown_members(with_member(case, path)), [f"{path or 'case'}: bogus"],
                                 f"{case['id']} at {path!r}")

    def test_a_member_of_another_kind_is_refused(self):
        self.assertEqual(manifest.unknown_members(with_member(SESSION, "", "raw")), ["case: raw"])
        self.assertEqual(manifest.unknown_members(with_member(PCM, "", "hex")), ["case: hex"])

    def test_a_selector_without_a_schema_is_refused(self):
        self.assertEqual(manifest.unknown_members({**SESSION, "kind": "telepathy"}), ["kind: 'telepathy' has no schema"])


if __name__ == "__main__":
    unittest.main()
