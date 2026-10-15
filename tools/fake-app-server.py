#!/usr/bin/env python3
"""A scripted stand-in for `codex app-server --listen stdio://`, run by both test suites.

Installed as `$binDir/codex`. Environment:

  SAYSO_FAKE_SCENARIO  path of a JSON scenario (a fixture's `setup.server`); required
  SAYSO_FAKE_RECORD    path of a JSON-lines record; one {"spawn": ...} line per start, one {"req": ...} line per
                       received message

Scenario keys (all optional):

  version              userAgent version, default "0.162.1"; null sends a userAgent without a version
  exitBeforeInitialize exit with this code before reading anything (Codex in a read-only CODEX_HOME)
  stderr               text written to stderr at start
  serverRequest        before the first getAuthStatus reply, send this server request (it has an id and a method);
                       the client's answer is recorded like any message
  descendant           start a process in its own session that holds stdout and stderr open for 30 s, and record
                       its pid as {"descendant": pid}
  ignoreTerm           ignore SIGTERM, so only SIGKILL ends the process
  methods              {method: [step, ...]}; call n of a method uses step n, and the last step repeats. A step is
                       {"result": ...} | {"error": {"code", "message"}} | {"raw": "line"} | {"exit": code} |
                       {"rawLineBytes": n} (one line of n bytes, then a newline),
                       optionally with "delayMs". A method with no steps answers -32601.
  byParams             {method: [{"when": {param: value}, "steps": [...]}]}: the first entry whose `when` matches the
                       call's params, by equality of each named key, is used instead of `methods`; each entry
                       counts its own calls

{"$jwt": {...claims}} anywhere in a reply is replaced by an unsigned JWT (spec/fixtures.md).
"""
import base64
import json
import os
import signal
import subprocess
import sys
import time


def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def jwt(claims: dict) -> str:
    header = b64url(b'{"alg":"none","typ":"JWT"}')
    payload = b64url(json.dumps(claims, separators=(",", ":"), sort_keys=True).encode())
    return f"{header}.{payload}.{b64url(b'sig')}"


def substitute(value):
    if isinstance(value, dict):
        if set(value) == {"$jwt"}:
            return jwt(value["$jwt"])
        return {k: substitute(v) for k, v in value.items()}
    if isinstance(value, list):
        return [substitute(v) for v in value]
    return value


def record(entry: dict) -> None:
    path = os.environ.get("SAYSO_FAKE_RECORD")
    if not path:
        return
    entry["pid"] = os.getpid()
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(entry) + "\n")


def send(message: dict) -> None:
    sys.stdout.write(json.dumps(substitute(message)) + "\n")
    sys.stdout.flush()


def main() -> int:
    with open(os.environ["SAYSO_FAKE_SCENARIO"], encoding="utf-8") as f:
        scenario = json.load(f)
    record({"spawn": {"argv": sys.argv[1:], "env": dict(os.environ)}})
    if scenario.get("ignoreTerm"):
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    if "exitBeforeInitialize" in scenario:
        return int(scenario["exitBeforeInitialize"])
    if scenario.get("stderr"):
        sys.stderr.write(scenario["stderr"])
        sys.stderr.flush()
    if scenario.get("descendant"):
        child = subprocess.Popen(["/bin/sleep", "30"], stdout=sys.stdout, stderr=sys.stderr, start_new_session=True)
        record({"descendant": child.pid})

    methods = scenario.get("methods", {})
    by_params = scenario.get("byParams", {})
    calls: dict = {}
    pending_server_request = scenario.get("serverRequest")

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            record({"malformedRequest": line})
            continue
        record({"req": msg})
        if "id" not in msg or "method" not in msg:
            continue  # a notification, or the client's answer to a server request
        method, params = msg["method"], msg.get("params") or {}
        if method == "initialize" and "initialize" not in methods:
            version = scenario.get("version", "0.162.1")
            agent = "codex_cli_rs" if version is None else f"codex_cli_rs/{version} (Fake 1.0; test)"
            send({"id": msg["id"], "result": {"userAgent": agent}})
            continue
        steps, key = methods.get(method), method
        for i, entry in enumerate(by_params.get(method, [])):
            if all(params.get(k) == v for k, v in entry["when"].items()):
                steps, key = entry["steps"], f"{method}#{i}"
                break
        if not steps:
            send({"id": msg["id"], "error": {"code": -32601, "message": f"Method not found: {method}"}})
            continue
        n = calls.get(key, 0)
        calls[key] = n + 1
        step = steps[min(n, len(steps) - 1)]
        if step.get("delayMs"):
            time.sleep(step["delayMs"] / 1000)
        if method == "getAuthStatus" and pending_server_request:
            send(pending_server_request)
            pending_server_request = None
        if "exit" in step:
            return int(step["exit"])
        if "rawLineBytes" in step:
            sys.stdout.write("x" * int(step["rawLineBytes"]) + "\n")
            sys.stdout.flush()
        elif "raw" in step:
            sys.stdout.write(step["raw"] + "\n")
            sys.stdout.flush()
        elif "error" in step:
            send({"id": msg["id"], "error": step["error"]})
        else:
            send({"id": msg["id"], "result": step.get("result", {})})
    return 0


if __name__ == "__main__":
    sys.exit(main())
