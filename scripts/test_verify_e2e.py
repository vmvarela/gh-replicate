"""Offline tests for verify-e2e.py using fake gh/git executables on PATH."""
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("verify-e2e.py")
FAKE_GH = """import json, os, sys
path = sys.argv[-1]
with open(os.environ["FAKE_LOG"], "a") as f:
    f.write(path + "\\n")
responses = json.load(open(os.environ["FAKE_RESPONSES"]))
if path not in responses:
    sys.stderr.write("gh: Not Found (HTTP 404)\\n")
    sys.exit(1)
r = responses[path]
if isinstance(r, dict) and "__error__" in r:
    sys.stderr.write(r["__error__"] + "\\n")
    sys.exit(1)
sys.stdout.write(r if isinstance(r, str) else json.dumps(r))
"""
FAKE_GIT = "import sys\n"  # ls-remote with no refs for both repos


def responses(**overrides):
    r = {
        "repos/o/s/releases?per_page=100&page=1": [],
        "repos/o/t/releases?per_page=100&page=1": [],
        "repos/o/s": {"description": "d", "homepage": "", "default_branch": "main"},
        "repos/o/t": {"description": "d", "homepage": "", "default_branch": "main"},
        "repos/o/t/actions/permissions": {"enabled": False},
        "repos/o/s/topics": {"names": []},
        "repos/o/t/topics": {"names": []},
        "repos/o/s/labels?per_page=100&page=1": [],
        "repos/o/t/labels?per_page=100&page=1": [],
    }
    r.update(overrides)
    return r


class VerifyE2ETest(unittest.TestCase):
    def run_script(self, resp, mode="verify"):
        d = pathlib.Path(self.enterContext(tempfile.TemporaryDirectory()))
        for name, code in (("gh", FAKE_GH), ("git", FAKE_GIT)):
            exe = d / name
            exe.write_text(f"#!{sys.executable}\n{code}")
            exe.chmod(0o755)
        (d / "resp.json").write_text(json.dumps({k: v for k, v in resp.items() if v is not None}))
        (d / "snap.json").write_text("{}")
        log = d / "log"
        log.touch()
        env = dict(os.environ, PATH=f"{d}{os.pathsep}{os.environ['PATH']}", SOURCE_REPOSITORY="o/s",
                   TARGET_REPOSITORY="o/t", FAKE_RESPONSES=str(d / "resp.json"), FAKE_LOG=str(log))
        args = [sys.executable, str(SCRIPT), mode] + ([str(d / "snap.json")] if mode == "verify" else [])
        p = subprocess.run(args, env=env, capture_output=True, text=True)
        return p, log.read_text().splitlines()

    def assertFails(self, resp, msg):
        p, _ = self.run_script(resp)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn(msg, p.stderr)

    def test_success(self):
        p, calls = self.run_script(responses())
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("repos/o/t/actions/permissions", calls)

    def test_snapshot_skips_new_checks(self):
        p, calls = self.run_script(responses(), mode="snapshot")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(calls, ["repos/o/t/releases?per_page=100&page=1"])

    def test_default_branch_mismatch(self):
        self.assertFails(responses(**{"repos/o/t": {"description": "d", "homepage": "", "default_branch": "dev"}}),
                         "default_branch differs")

    def test_default_branch_missing_or_malformed(self):
        for bad in ({}, {"default_branch": ""}, {"default_branch": None}, {"default_branch": 3}):
            with self.subTest(bad=bad):
                self.assertFails(responses(**{"repos/o/s": {"description": "d", "homepage": "", **bad}}),
                                 "default_branch missing or malformed")

    def test_default_branch_whitespace_in_both(self):
        blank = {"description": "d", "homepage": "", "default_branch": "  "}
        self.assertFails(responses(**{"repos/o/s": blank, "repos/o/t": blank}),
                         "default_branch missing or malformed")

    def test_actions_enabled(self):
        self.assertFails(responses(**{"repos/o/t/actions/permissions": {"enabled": True}}), "enabled=True")

    def test_actions_missing_or_malformed(self):
        for bad in ({}, {"enabled": "false"}, {"enabled": 0}, {"enabled": None}):
            with self.subTest(bad=bad):
                self.assertFails(responses(**{"repos/o/t/actions/permissions": bad}), "Actions must be disabled")
        self.assertFails(responses(**{"repos/o/t/actions/permissions": []}), "malformed response")
        self.assertFails(responses(**{"repos/o/t/actions/permissions": "not json"}), "failed")

    def test_api_errors(self):
        self.assertFails(responses(**{"repos/o/t/actions/permissions": {"__error__": "HTTP 403"}}),
                         "API repos/o/t/actions/permissions failed")
        self.assertFails(responses(**{"repos/o/t/actions/permissions": None}), "failed")
        self.assertFails(responses(**{"repos/o/s": {"__error__": "HTTP 500"}}), "API repos/o/s failed")


if __name__ == "__main__":
    unittest.main()
