"""Tests for tools/fake-app-server.py, the scripted codex both suites run."""
import base64
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
FAKE = ROOT / "tools" / "fake-app-server.py"


def converse(scenario, messages, args=("-c", 'model_provider="openai"', "app-server", "--listen", "stdio://")):
    with tempfile.TemporaryDirectory() as d:
        sc, rec = pathlib.Path(d, "s.json"), pathlib.Path(d, "r.jsonl")
        sc.write_text(json.dumps(scenario))
        env = dict(os.environ, SAYSO_FAKE_SCENARIO=str(sc), SAYSO_FAKE_RECORD=str(rec))
        p = subprocess.run([sys.executable, "-I", str(FAKE), *args], input="".join(json.dumps(m) + "\n" for m in messages),
                           capture_output=True, text=True, env=env, timeout=20)
        lines = [json.loads(l) for l in rec.read_text().splitlines()] if rec.exists() else []
        return p, [json.loads(l) for l in p.stdout.splitlines() if l.startswith("{")], p.stdout, lines


class Fake(unittest.TestCase):
    def test_hold_after_eof_and_group_descendant(self):
        import signal, time
        with tempfile.TemporaryDirectory() as d:
            sc, rec = pathlib.Path(d, "s.json"), pathlib.Path(d, "r.jsonl")
            sc.write_text(json.dumps({"holdAfterEOF": True, "groupDescendant": True}))
            env = dict(os.environ, SAYSO_FAKE_SCENARIO=str(sc), SAYSO_FAKE_RECORD=str(rec))
            p = subprocess.Popen([sys.executable, "-I", str(FAKE)], stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                                 env=env, start_new_session=True)
            try:
                p.stdin.close()
                deadline = time.time() + 10
                def recorded():
                    return rec.exists() and any("groupDescendant" in l for l in rec.read_text().splitlines())
                while time.time() < deadline and not recorded():
                    time.sleep(0.05)
                desc = [json.loads(l)["groupDescendant"] for l in rec.read_text().splitlines() if "groupDescendant" in l][0]
                time.sleep(0.3)
                self.assertIsNone(p.poll(), "the fake ended at stdin EOF")
                self.assertEqual(os.getpgid(desc), p.pid)
                os.killpg(p.pid, signal.SIGTERM)
                time.sleep(0.3)
                os.kill(desc, 0)  # still alive: it ignores SIGTERM
            finally:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait(timeout=5)

    def test_initialize_and_jwt_substitution(self):
        scenario = {"byParams": {"getAuthStatus": [{"when": {"includeToken": True}, "steps": [{"result": {"authMethod": "chatgpt", "authToken": {"$jwt": {"exp": 5}}}}]}]}}
        p, out, _, rec = converse(scenario, [{"id": 1, "method": "initialize", "params": {}}, {"method": "initialized"},
                                             {"id": 2, "method": "getAuthStatus", "params": {"includeToken": True, "refreshToken": False}}])
        self.assertEqual(p.returncode, 0)
        self.assertEqual(out[0]["result"]["userAgent"], "codex_cli_rs/0.162.1 (Fake 1.0; test)")
        token = out[1]["result"]["authToken"]
        payload = token.split(".")[1]
        self.assertEqual(json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4))), {"exp": 5})
        self.assertEqual(rec[0]["spawn"]["argv"], ["-c", 'model_provider="openai"', "app-server", "--listen", "stdio://"])
        self.assertEqual([r["req"]["method"] for r in rec[1:]], ["initialize", "initialized", "getAuthStatus"])

    def test_notify_around_a_reply(self):
        scenario = {"methods": {"m": [{"notify": [{"method": "a", "params": {"n": "$i"}, "repeat": 3},
                                                  {"id": "srv", "method": "approve"}, {"method": "pad", "padBytes": 4}],
                                       "result": {}, "notifyAfter": [{"method": "cwd", "params": {"cwd": "$cwd"}}]}]}}
        _, out, _, _ = converse(scenario, [{"id": 1, "method": "m"}])
        self.assertEqual([o.get("method") for o in out], ["a", "a", "a", "approve", "pad", None, "cwd"])
        self.assertEqual([o["params"]["n"] for o in out[:3]], [0, 1, 2])
        self.assertEqual(out[3]["id"], "srv")
        self.assertEqual(out[4]["params"], {"pad": "xxxx"})
        self.assertEqual(out[5], {"id": 1, "result": {}})
        self.assertEqual(out[6]["params"]["cwd"], os.getcwd())

    def test_unscripted_method_is_not_found(self):
        _, out, _, _ = converse({}, [{"id": 1, "method": "config/read", "params": {}}])
        self.assertEqual(out[0]["error"]["code"], -32601)

    def test_steps_advance_and_last_repeats(self):
        scenario = {"methods": {"m": [{"result": {"n": 1}}, {"error": {"code": -1, "message": "e"}}]}}
        _, out, _, _ = converse(scenario, [{"id": i, "method": "m"} for i in range(3)])
        self.assertEqual([("result" in o, "error" in o) for o in out], [(True, False), (False, True), (False, True)])

    def test_steps_count_across_starts_sharing_a_record(self):
        scenario = {"methods": {"m": [{"result": {"n": 1}}, {"result": {"n": 2}}]}}
        with tempfile.TemporaryDirectory() as d:
            sc, rec = pathlib.Path(d, "s.json"), pathlib.Path(d, "r.jsonl")
            sc.write_text(json.dumps(scenario))
            env = dict(os.environ, SAYSO_FAKE_SCENARIO=str(sc), SAYSO_FAKE_RECORD=str(rec))
            seen = []
            for _ in range(3):
                p = subprocess.run([sys.executable, "-I", str(FAKE)], input=json.dumps({"id": 1, "method": "m"}) + "\n",
                                   capture_output=True, text=True, env=env, timeout=20)
                seen.append(json.loads(p.stdout.splitlines()[0])["result"]["n"])
        self.assertEqual(seen, [1, 2, 2])

    def test_steps_advance_without_a_record(self):
        with tempfile.TemporaryDirectory() as d:
            sc = pathlib.Path(d, "s.json")
            sc.write_text(json.dumps({"methods": {"m": [{"result": {"n": 1}}, {"result": {"n": 2}}]}}))
            env = {k: v for k, v in os.environ.items() if k != "SAYSO_FAKE_RECORD"}
            env["SAYSO_FAKE_SCENARIO"] = str(sc)
            p = subprocess.run([sys.executable, "-I", str(FAKE)], input="".join(json.dumps({"id": i, "method": "m"}) + "\n" for i in range(3)),
                               capture_output=True, text=True, env=env, timeout=20)
        self.assertEqual([json.loads(l)["result"]["n"] for l in p.stdout.splitlines()], [1, 2, 2])

    def test_server_request_comes_first(self):
        scenario = {"serverRequest": {"id": "srv-1", "method": "execCommandApproval", "params": {}}, "methods": {"getAuthStatus": [{"result": {}}]}}
        _, out, _, _ = converse(scenario, [{"id": 1, "method": "getAuthStatus", "params": {}}])
        self.assertEqual(out[0]["method"], "execCommandApproval")
        self.assertEqual(out[1]["id"], 1)

    def test_exit_before_initialize(self):
        p, out, _, rec = converse({"exitBeforeInitialize": 1}, [{"id": 1, "method": "initialize"}])
        self.assertEqual((p.returncode, out), (1, []))
        self.assertIn("spawn", rec[0])

    def test_pad_line_to(self):
        _, out, stdout, _ = converse({"methods": {"m": [{"result": {"a": 1}, "padLineTo": 1000}, {"error": {"code": 1, "message": "e"}, "padLineTo": 999}]}},
                                     [{"id": 1, "method": "m"}, {"id": 2, "method": "m"}])
        first, second = stdout.split("\n")[:2]
        self.assertEqual((len(first), len(second)), (1000, 999))
        self.assertEqual((first.rstrip(" "), second.rstrip(" ")), ('{"id": 1, "result": {"a": 1}}', '{"id": 2, "error": {"code": 1, "message": "e"}}'))
        self.assertEqual(out[0]["result"], {"a": 1})

    def test_pad_line_to_refuses_a_longer_reply(self):
        p, _, stdout, _ = converse({"methods": {"m": [{"result": {"a": 1}, "padLineTo": 5}]}}, [{"id": 1, "method": "m"}])
        self.assertNotEqual(p.returncode, 0)
        self.assertEqual(stdout, "")

    def test_raw_then_exit(self):
        p, _, stdout, _ = converse({"methods": {"m": [{"rawThenExit": '{"id":1,"result":{}}'}]}}, [{"id": 1, "method": "m"}, {"id": 2, "method": "m"}])
        self.assertEqual(p.returncode, 0)
        self.assertEqual(stdout, '{"id":1,"result":{}}')  # no newline, and nothing after it

    def test_no_version(self):
        _, out, _, _ = converse({"version": None}, [{"id": 1, "method": "initialize"}])
        self.assertEqual(out[0]["result"]["userAgent"], "codex_cli_rs")


if __name__ == "__main__":
    unittest.main()
