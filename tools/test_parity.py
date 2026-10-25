"""Tests for tools/parity.py: each way a suite could look green without running its fixtures must fail."""
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
MANIFEST = json.loads((ROOT / "fixtures" / "MANIFEST.json").read_text(encoding="utf-8"))
POLICY = json.loads((ROOT / "fixtures" / "capabilities.json").read_text(encoding="utf-8"))


def full(impl):
    need = POLICY["implementations"][impl]
    return {"implementation": impl, "components": need["components"], "capabilities": need["capabilities"],
            "results": {f["id"]: "pass" for f in MANIFEST["fixtures"]}}


def run(*docs, raw=None):
    with tempfile.TemporaryDirectory() as d:
        paths = []
        for i, doc in enumerate(docs):
            p = pathlib.Path(d, f"r{i}.json")
            p.write_text(raw if raw is not None else json.dumps(doc), encoding="utf-8")
            paths.append(str(p))
        return subprocess.run([sys.executable, "-I", str(ROOT / "tools" / "parity.py"), *paths], capture_output=True, text=True)


class Parity(unittest.TestCase):
    def test_complete_results_pass(self):
        self.assertEqual(run(full("go"), full("swift")).returncode, 0)

    def test_no_results_fail(self):
        self.assertNotEqual(subprocess.run([sys.executable, "-I", str(ROOT / "tools" / "parity.py")], capture_output=True).returncode, 0)

    def test_missing_id_fails(self):
        r = full("go")
        r["results"].pop(MANIFEST["fixtures"][0]["id"])
        self.assertIn("was not run", run(r).stderr)

    def test_dropped_component_fails(self):
        r = full("go")
        r["components"] = [c for c in r["components"] if c != "pcm16"]
        r["results"] = {k: v for k, v in r["results"].items() if not k.startswith("pcm-")}
        self.assertIn("must implement component pcm16", run(r).stderr)

    def test_dropped_capability_fails(self):
        r = full("swift")
        r["capabilities"] = r["capabilities"][1:]
        self.assertIn("must declare capability", run(r).stderr)

    def test_unknown_id_fails(self):
        r = full("go")
        r["results"]["no-such-fixture"] = "pass"
        self.assertIn("unknown fixture id", run(r).stderr)

    def test_duplicate_id_fails(self):
        fid = MANIFEST["fixtures"][0]["id"]
        doc = json.dumps(full("go"))
        doc = doc.replace('"results": {', '"results": {"%s": "pass", ' % fid, 1)
        self.assertIn("duplicate id", run(None, raw=doc).stderr)

    def test_failure_fails(self):
        r = full("go")
        r["results"][MANIFEST["fixtures"][0]["id"]] = "fail"
        self.assertIn("failed", run(r).stderr)

    def test_a_failure_in_an_undeclared_component_fails(self):
        r = full("go")
        r["components"] = [c for c in r["components"] if c != "pcm16"]
        fid = next(f["id"] for f in MANIFEST["fixtures"] if f["component"] == "pcm16")
        r["results"][fid] = "fail"
        self.assertIn(f"fixture {fid} failed", run(r).stderr)

    def test_a_malformed_results_file_is_reported_and_the_rest_checked(self):
        for doc in ([], {**full("go"), "results": []}, {**full("go"), "components": "pcm16"},
                    {**full("go"), "implementation": ["go"]}):
            r = run(doc, {**full("swift"), "results": {}})
            self.assertEqual(r.returncode, 1, doc)
            self.assertNotIn("Traceback", r.stderr, doc)
            self.assertIn("r0.json", r.stderr, doc)
            self.assertIn("swift: fixture", r.stderr, doc)

    def test_an_unreadable_results_file_is_reported_and_the_rest_checked(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d, "swift.json")
            p.write_text(json.dumps({**full("swift"), "results": {}}), encoding="utf-8")
            r = subprocess.run([sys.executable, "-I", str(ROOT / "tools" / "parity.py"), str(pathlib.Path(d, "absent.json")),
                                str(p)], capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)
        self.assertNotIn("Traceback", r.stderr)
        self.assertIn("absent.json", r.stderr)
        self.assertIn("swift: fixture", r.stderr)

    def test_exempt_only_for_true_said_guard_with_missing_capability(self):
        cases = {c["id"]: c for c in json.loads((ROOT / "fixtures" / "said-guard.json").read_text(encoding="utf-8"))}
        exemptable = next(f["id"] for f in MANIFEST["fixtures"] if f["requires"] and cases[f["id"]]["expect"] is True)
        r = full("go")
        r["results"][exemptable] = "exempt"
        self.assertEqual(run(r).returncode, 0, run(r).stderr)
        swift = full("swift")
        swift["results"][exemptable] = "exempt"
        self.assertIn("may not be exempt", run(swift).stderr, "Swift declares the capability, so it must pass")
        false_case = next(f["id"] for f in MANIFEST["fixtures"] if f["component"] == "said-guard" and cases[f["id"]]["expect"] is False)
        r = full("go")
        r["results"][false_case] = "exempt"
        self.assertIn("may not be exempt", run(r).stderr)
        r = full("go")
        r["results"]["pcm-encode"] = "exempt"
        self.assertIn("may not be exempt", run(r).stderr)


if __name__ == "__main__":
    unittest.main()
