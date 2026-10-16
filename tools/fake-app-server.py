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
  holdAfterEOF         stay alive after stdin ends, so closing stdin does not end the process either
  groupDescendant      start a process in the fake's own process group that ignores SIGTERM and lives 30 s, with no
                       pipe of ours, and record its pid as {"groupDescendant": pid}: only SIGKILL to the group ends it
  methods              {method: [step, ...]}; call n of a method uses step n, and the last step repeats. Calls are
                       counted across starts that share SAYSO_FAKE_RECORD, because a client starts one child per
                       fetch or refresh. A step is
                       {"result": ...} | {"error": {"code", "message"}} | {"raw": "line"} | {"exit": code} |
                       {"rawThenExit": "text"} (the text with no newline, then exit 0: a last line ended by EOF),
                       optionally with "padLineTo": n (a result or error reply padded with trailing spaces, JSON's own
                       whitespace, to exactly n bytes before its newline), "delayMs", "notify" (messages sent before the reply), "awaitLines": n (after
                       the notify messages, read and record n more lines of input before going on: a barrier that
                       proves the client wrote them while this call waits) and "notifyAfter" (sent after it). A message is {"method", "params"} (a notification) or carries an "id" too (a server
                       request); "repeat": n sends it n times with "$i" in a params string value replaced by the index
                       (a number), and "padBytes": n adds a params member "pad" of n "x". The string "$cwd" in a message is
                       replaced by the fake's working directory. {"rawLine": "text"} sends the text as one line, byte
                       for byte. A method with no steps answers -32601.
  byParams             {method: [{"when": {param: value}, "steps": [...]}]}: the first entry whose `when` matches the
                       call's params, by equality of each named key, is used instead of `methods`; each entry
                       counts its own calls, across starts like `methods`

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


MEMORY: list = []  # the requests of this start, when there is no record


def record(entry: dict) -> None:
    path = os.environ.get("SAYSO_FAKE_RECORD")
    if not path:
        if "req" in entry:
            MEMORY.append(entry["req"])
        return
    entry["pid"] = os.getpid()
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(entry) + "\n")


def recorded_requests() -> list:
    """Every request in the record so far, across starts; this start's requests when there is no record."""
    path = os.environ.get("SAYSO_FAKE_RECORD")
    if not path:
        return MEMORY
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as f:
        return [e["req"] for e in map(json.loads, f) if "req" in e]


def step_key(msg: dict, methods: dict, by_params: dict):
    """The steps a request is answered from, and the key its calls are counted under."""
    method, params = msg["method"], msg.get("params") or {}
    for i, entry in enumerate(by_params.get(method, [])):
        if all(params.get(k) == v for k, v in entry["when"].items()):
            return entry["steps"], f"{method}#{i}"
    return methods.get(method), method


def send(message: dict, pad_to: int = 0) -> None:
    line = json.dumps(substitute(message))  # ASCII only, so its length is its size in bytes
    if len(line) > pad_to > 0:
        sys.exit(f"fake-app-server: a reply of {len(line)} bytes cannot be padded to {pad_to}")
    sys.stdout.write(line.ljust(pad_to) + "\n")
    sys.stdout.flush()


def indexed(value, i: int):
    """value with every string "$i" replaced by i and "$cwd" by the working directory."""
    if isinstance(value, dict):
        return {k: indexed(v, i) for k, v in value.items()}
    if isinstance(value, list):
        return [indexed(v, i) for v in value]
    if value == "$i":
        return i
    if value == "$cwd":
        return os.getcwd()
    return value


def notify(messages: list) -> None:
    """Sends a step's scripted notifications and server requests, in order."""
    for spec in messages:
        if "rawLine" in spec:
            sys.stdout.write(spec["rawLine"] + "\n")
            sys.stdout.flush()
            continue
        spec = dict(spec)
        repeat, pad = int(spec.pop("repeat", 1)), int(spec.pop("padBytes", 0))
        for i in range(repeat):
            message = indexed(spec, i)
            if pad:
                message["params"] = dict(message.get("params") or {}, pad="x" * pad)
            send(message)


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
    if scenario.get("groupDescendant"):
        # SIG_IGN set before exec survives it, so the sleep ignores SIGTERM from its first instruction.
        child = subprocess.Popen(["/bin/sleep", "30"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL, preexec_fn=lambda: signal.signal(signal.SIGTERM, signal.SIG_IGN))
        record({"groupDescendant": child.pid})

    methods = scenario.get("methods", {})
    by_params = scenario.get("byParams", {})
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
        method = msg["method"]
        if method == "initialize" and "initialize" not in methods:
            version = scenario.get("version", "0.162.1")
            agent = "codex_cli_rs" if version is None else f"codex_cli_rs/{version} (Fake 1.0; test)"
            send({"id": msg["id"], "result": {"userAgent": agent}})
            continue
        steps, key = step_key(msg, methods, by_params)
        if not steps:
            send({"id": msg["id"], "error": {"code": -32601, "message": f"Method not found: {method}"}})
            continue
        # This request is already recorded, so the earlier calls are the matches before the last one.
        n = sum(1 for r in recorded_requests() if "id" in r and "method" in r and step_key(r, methods, by_params)[1] == key) - 1
        step = steps[min(n, len(steps) - 1)]
        if step.get("delayMs"):
            time.sleep(step["delayMs"] / 1000)
        notify(step.get("notify", []))
        for _ in range(int(step.get("awaitLines", 0))):
            waited = sys.stdin.readline()
            if not waited:
                return 0  # the client closed our input first
            try:
                record({"req": json.loads(waited)})
            except json.JSONDecodeError:
                record({"malformedRequest": waited.strip()})
        if method == "getAuthStatus" and pending_server_request:
            send(pending_server_request)
            pending_server_request = None
        if "exit" in step:
            return int(step["exit"])
        if "rawThenExit" in step:
            sys.stdout.write(step["rawThenExit"])
            sys.stdout.flush()
            return 0
        if "raw" in step:
            sys.stdout.write(step["raw"] + "\n")
            sys.stdout.flush()
        elif "error" in step:
            send({"id": msg["id"], "error": step["error"]}, int(step.get("padLineTo", 0)))
        else:
            send({"id": msg["id"], "result": step.get("result", {})}, int(step.get("padLineTo", 0)))
        notify(step.get("notifyAfter", []))
    if scenario.get("holdAfterEOF"):
        time.sleep(30)
    return 0


if __name__ == "__main__":
    sys.exit(main())
