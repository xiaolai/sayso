#!/usr/bin/env python3
"""Check every implementation's results against the fixture inventory and policy (spec/parity.md).

Usage: tools/parity.py results-go.json results-swift.json ...

Fails when the manifest is stale, empty or has duplicates; when an implementation declares less than
capabilities.json requires; when a result set misses an id of a declared component, has an unknown id or a
duplicate; when an outcome is "fail"; or when an "exempt" outcome is not a true said-guard expectation whose
`requires` the implementation does not declare.
"""
import json
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
FIXTURES = ROOT / "fixtures"


def fail(problems):
    for p in problems:
        print("parity:", p, file=sys.stderr)
    sys.exit(1)


def duplicate_keys(pairs):
    seen = {}
    for k, v in pairs:
        if k in seen:
            raise ValueError(f"duplicate id {k}")
        seen[k] = v
    return seen


def main(paths):
    if not paths:
        fail(["no results files given"])
    check = subprocess.run([sys.executable, "-I", str(ROOT / "tools" / "manifest.py"), "--check"], capture_output=True, text=True)
    if check.returncode != 0:
        fail([check.stderr.strip() or "fixtures/MANIFEST.json is stale"])
    manifest = json.loads((FIXTURES / "MANIFEST.json").read_text(encoding="utf-8"))
    if manifest["count"] == 0 or manifest["count"] != len(manifest["fixtures"]):
        fail(["manifest is empty or its count is wrong"])
    by_id = {f["id"]: f for f in manifest["fixtures"]}
    if len(by_id) != len(manifest["fixtures"]):
        fail(["manifest has a duplicate id"])
    policy = json.loads((FIXTURES / "capabilities.json").read_text(encoding="utf-8"))
    expectations = {}
    for f in {f["file"] for f in manifest["fixtures"]}:
        path = FIXTURES / f
        text = path.read_text(encoding="utf-8")
        cases = [json.loads(l) for l in text.splitlines() if l.strip()] if path.suffix == ".jsonl" else json.loads(text)
        for c in cases:
            expectations[c["id"]] = c.get("expect")

    problems = []
    for p in paths:
        try:
            r = json.loads(pathlib.Path(p).read_text(encoding="utf-8"), object_pairs_hook=duplicate_keys)
        except (OSError, ValueError) as e:
            problems.append(f"{p}: {e}")
            continue
        if not isinstance(r, dict):
            problems.append(f"{p}: not a JSON object")
            continue
        impl, results = r.get("implementation"), r.get("results", {})
        components, capabilities = r.get("components", []), r.get("capabilities", [])
        if not (isinstance(impl, str) and isinstance(results, dict)
                and all(isinstance(x, list) and all(isinstance(s, str) for s in x) for x in (components, capabilities))):
            problems.append(f"{p}: implementation must be a string, components and capabilities lists of strings, "
                            "and results an object")
            continue
        need = policy["implementations"].get(impl)
        if need is None:
            problems.append(f"{p}: unknown implementation {impl!r}")
            continue
        components, capabilities = set(components), set(capabilities)
        for c in set(need["components"]) - components:
            problems.append(f"{impl}: must implement component {c}")
        for c in set(need["capabilities"]) - capabilities:
            problems.append(f"{impl}: must declare capability {c}")
        for fid in results:
            if fid not in by_id:
                problems.append(f"{impl}: unknown fixture id {fid}")
        for fid, f in by_id.items():
            outcome = results.get(fid)
            if outcome is None:
                # Only a declared component must run every fixture; any outcome given is checked all the same.
                if f["component"] in components:
                    problems.append(f"{impl}: fixture {fid} was not run")
            elif outcome == "fail":
                problems.append(f"{impl}: fixture {fid} failed")
            elif outcome == "exempt":
                missing = set(f["requires"]) - capabilities
                if not (f["component"] == "said-guard" and expectations[fid] is True and missing):
                    problems.append(f"{impl}: fixture {fid} may not be exempt")
            elif outcome != "pass":
                problems.append(f"{impl}: fixture {fid} has outcome {outcome!r}")
    if problems:
        fail(problems)
    print(f"parity: {len(paths)} implementation(s), {manifest['count']} fixtures, all consistent")


if __name__ == "__main__":
    main(sys.argv[1:])
