#!/usr/bin/env python3
"""Write fixtures/MANIFEST.json: every fixture id with its file, component and requires, and the total.

The manifest is the inventory the test suites are checked against, independent of the suites themselves
(spec: parity). Run with --check to fail when the checked-in manifest differs from a fresh scan. Either way every
case is checked against SCHEMA first: a member spec/fixtures.md does not define is refused, because no harness would
read it and the case would pass whatever it says.
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
FIXTURES = ROOT / "fixtures"
SKIP = {"MANIFEST.json", "capabilities.json"}

# The members every case may carry besides its component's own.
HEAD = {"id", "component", "note", "requires"}

CREDENTIAL_EXPECT = {
    "state", "class", "token", "accountId", "expiresAt", "mode", "codexVersion", "spawns", "argv", "requests", "envAbsent",
    "envEquals", "pathStartsWith", "binDir", "childrenExited", "closeWithinMs", "serverRequestAnswered", "outbound",
    "refreshCalls", "notInDescription", "retryAt", "inDescription", "inLog", "stderr",
    # client cases (setup.client)
    "startFails", "calls", "callError", "events", "eventCount", "eventSequence", "typedResults",
    # private sign-in cases (setup.privateSignIn)
    "signIn", "fileMode", "entries", "elsewhere",
}
AUTH_FILE = {"kind", "json", "raw", "hex", "rawRepeat", "padTo"}
ERROR_EXPECT = {"class", "fatal", "code", "type", "param", "notInDescription"}

# Per component, the members each object of a case may hold (spec/fixtures.md), by path: "" is the case itself,
# "a.b" the object at a.b, "a[]" each object in the list at a. A component whose cases differ by a selector member
# (session's kind, pcm16's op) lists a schema per value under "cases"; "common" applies to every value.
SCHEMA = {
    "credentials": {
        "": {"setup", "expect", "steps"},
        "setup": {"now", "marginSeconds", "source", "codex", "inheritedPath", "inheritedEnv", "codexHome", "file",
                  "retry", "timeoutMs", "cancelOnRequest", "server", "client", "privateSignIn"},
        "setup.privateSignIn": {"token", "folder"},
        "setup.file": AUTH_FILE,
        "setup.file.rawRepeat": {"text", "count"},
        "setup.retry": AUTH_FILE,
        "setup.retry.rawRepeat": {"text", "count"},
        "setup.client": {"handler", "dir", "experimentalAPI", "calls"},
        "setup.client.calls[]": {"method", "params", "typed"},
        "expect": CREDENTIAL_EXPECT,
        "expect.requests[]": {"method", "params", "notification"},
        "expect.events[]": {"method", "serverRequest", "noParams", "params", "paramsRaw"},
        "steps[]": {"at", "op", "stale", "expect", "note"},
        "steps[].expect": CREDENTIAL_EXPECT,
    },
    "mint": {
        "": {"setup", "expect"},
        "setup": {"now", "origin", "session", "lifetimeSeconds", "token", "refreshTo", "replies"},
        "setup.replies[]": {"status", "headers", "json", "body", "bodyPadTo", "hangup"},
        "expect": {"class", "secret", "expiresAt", "status", "type", "code", "param", "requestId", "requests",
                   "refreshCalls", "tokenCalls", "request", "secondAuthorization", "scrubs", "notInDescription"},
        "expect.request": {"method", "path", "authorization", "contentType", "body"},
    },
    "session": {
        "by": "kind",
        "common": {"": {"expect"}},
        "cases": {
            "close": {"": {"code", "reason"}, "expect": ERROR_EXPECT},
            "error-event": {"": {"event"}, "expect": ERROR_EXPECT},
            "size": {"": {"readLimit", "messageBytes"}, "expect": ERROR_EXPECT},
            "unknown-types": {"": {"types", "typeBytes", "rounds"}, "expect": ERROR_EXPECT | {"delivered", "distinct"}},
            "event": {"": {"raw"}, "expect": {
                "type", "sessionType", "model", "responseId", "metadata", "pcmHex", "delta", "status",
                "statusReason", "usage", "limits", "exhausted", "class", "code", "param", "itemId", "transcript",
                "ignored", "unknown", "protocolError", "notInDescription"}},
            "handshake": {
                "": {"reply", "silent", "handshakeTimeoutMs"},
                "reply": {"status", "headers", "body", "hold"},
                "expect": {"class", "status", "requestId", "code", "withinMs", "request", "notInDescription"},
                "expect.request": {"target", "authorization"},
            },
        },
    },
    "pcm16": {
        "by": "op",
        "common": {},
        "cases": {
            "encode": {"": {"input", "expectSamples", "expectHex"}},
            "decode": {"": {"hex", "sliceFrom", "expectSamples"}},
            "wav": {"": {"rate", "pcmHex", "expectHex", "expectError"}},
            "validWAV": {"": {"hex", "capBytes", "expect"}},
            "plausible": {"": {"pcm", "script", "expect"}, "pcm": {"samples", "value"}},
        },
    },
    "said-guard": {"": {"script", "transcript", "language", "expect"}},
}


def objects(case, path):
    """The objects at path in case (see SCHEMA); a member that is absent or not an object yields none."""
    found = [case]
    for part in path.split(".") if path else []:
        listed = part.endswith("[]")
        name = part[:-2] if listed else part
        values = [o.get(name) for o in found]
        if listed:
            found = [x for v in values if isinstance(v, list) for x in v if isinstance(x, dict)]
        else:
            found = [v for v in values if isinstance(v, dict)]
    return found


def unknown_members(case):
    """Each member of case outside its component's schema, as "path: key"; a selector value with no schema too."""
    schema = SCHEMA.get(case.get("component"), {})
    allowed = {"": set()}
    if "by" in schema:
        selector = case.get(schema["by"])
        if selector not in schema["cases"]:
            return [f"{schema['by']}: {selector!r} has no schema"]
        for part in (schema["common"], schema["cases"][selector]):
            for path, keys in part.items():
                allowed[path] = allowed.get(path, set()) | keys
        allowed[""] |= {schema["by"]}
    else:
        allowed.update(schema)
    allowed[""] = allowed[""] | HEAD
    problems = []
    for path, keys in sorted(allowed.items()):
        for obj in objects(case, path):
            problems += [f"{path or 'case'}: {key}" for key in sorted(obj) if key not in keys]
    return problems


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
            unknown = unknown_members(case)
            if unknown:
                sys.exit(f"{path.relative_to(FIXTURES)}: fixture {fid} has members spec/fixtures.md does not define: "
                         + ", ".join(unknown))
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
