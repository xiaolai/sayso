#!/usr/bin/env python3
"""Write fixtures/MANIFEST.json: every fixture id with its file, component and requires, and the total.

The manifest is the inventory the test suites are checked against, independent of the suites themselves
(spec: parity). Run with --check to fail when the checked-in manifest differs from a fresh scan.
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
FIXTURES = ROOT / "fixtures"
SKIP = {"MANIFEST.json", "capabilities.json"}


def load(path):
    """A .json file is an array of cases; a .jsonl file is one case per line."""
    text = path.read_text(encoding="utf-8")
    if path.suffix == ".jsonl":
        return [json.loads(line) for line in text.splitlines() if line.strip()]
    return json.loads(text)


def scan():
    entries, seen = [], set()
    files = sorted(p for p in FIXTURES.rglob("*") if p.suffix in (".json", ".jsonl") and p.name not in SKIP)
    if not files:
        sys.exit("no fixture files under fixtures/")
    components = set(json.loads((FIXTURES / "capabilities.json").read_text(encoding="utf-8"))["components"])
    for path in files:
        for case in load(path):
            if case.get("component") not in components:
                sys.exit(f"{path.relative_to(FIXTURES)}: fixture {case.get('id')} names no known component")
            fid = case["id"]
            if fid in seen:
                sys.exit(f"duplicate fixture id {fid}")
            seen.add(fid)
            entries.append({"id": fid, "file": str(path.relative_to(FIXTURES)), "component": case["component"],
                            "requires": case.get("requires", [])})
    return {"count": len(entries), "fixtures": entries}


def main():
    fresh = json.dumps(scan(), ensure_ascii=False, indent=1) + "\n"
    target = FIXTURES / "MANIFEST.json"
    if sys.argv[1:] == ["--check"]:
        if not target.exists() or target.read_text(encoding="utf-8") != fresh:
            sys.exit("fixtures/MANIFEST.json is stale; run tools/manifest.py")
        return
    target.write_text(fresh, encoding="utf-8")


if __name__ == "__main__":
    main()
