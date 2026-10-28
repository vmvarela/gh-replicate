#!/usr/bin/env python3
"""Self-check for check-security-sarif.py. Run: python3 scripts/test_check_security_sarif.py"""
import importlib.util
import json
import os
import tempfile

spec = importlib.util.spec_from_file_location(
    "chk", os.path.join(os.path.dirname(__file__), "check-security-sarif.py"))
chk = importlib.util.module_from_spec(spec)
spec.loader.exec_module(chk)

DRIVER = {"name": "gosec", "version": "dev", "informationUri": "https://github.com/securego/gosec/"}


def sarif(*runs, version="2.1.0"):
    return json.dumps({"version": version, "runs": list(runs)})


# Shape observed from real gosec v2.30.0 output on a clean module.
SARIF = sarif({"results": [], "tool": {"driver": DRIVER}, "taxonomies": [{"name": "CWE"}]})
FINDINGS = sarif({"results": [{"ruleId": "G304", "level": "error"}], "tool": {"driver": DRIVER}})

with tempfile.TemporaryDirectory() as d:
    def case(js, sarif):
        j, s = os.path.join(d, "g.json"), os.path.join(d, "g.sarif")
        for p, c in ((j, js), (s, sarif)):
            if os.path.exists(p):
                os.remove(p)
            if c is not None:
                open(p, "w").write(c)
        return chk.check(j, s)

    ok = json.dumps({"Golang errors": {}, "Issues": []})
    assert case(ok, SARIF) is None, "complete clean analysis"
    assert case(json.dumps({"Golang errors": {}, "Issues": [{}]}), FINDINGS) is None, "findings still upload"
    assert "analyze" in case(json.dumps({"Golang errors": {"x.go": [{}]}}), SARIF), "go errors"
    assert case(None, SARIF), "missing json"
    assert case("", SARIF), "empty json"
    assert case("{bad", SARIF), "corrupt json"
    assert case("{}", SARIF), "json without errors field"
    assert case(ok, None), "missing sarif"
    assert case(ok, ""), "empty sarif"
    assert case(ok, "{bad"), "corrupt sarif"
    assert case(ok, json.dumps({"runs": []})), "sarif without runs"
    assert case(json.dumps({"Golang errors": False}), SARIF), "errors not an object"
    assert case(json.dumps({"Golang errors": []}), SARIF), "errors as list"
    assert case(json.dumps({"Golang errors": None}), SARIF), "errors null"
    assert case(ok, sarif({})), "run without tool/results"
    assert case(ok, sarif(None)), "null run"
    assert case(ok, json.dumps([])), "sarif not an object"
    assert case(ok, sarif({"results": [], "tool": {"driver": DRIVER}}, version="2.0.0")), "wrong version"
    assert case(ok, sarif({"results": [], "tool": {"driver": {"name": "other"}}})), "other tool"
    assert case(ok, sarif({"results": [], "tool": None})), "null tool"
    assert case(ok, sarif({"tool": {"driver": DRIVER}})), "missing results"
    assert case(ok, sarif({"results": {}, "tool": {"driver": DRIVER}})), "results not a list"
    assert case(ok, sarif({"results": [None], "tool": {"driver": DRIVER}})), "null result"
    assert case(ok, sarif({"results": [], "tool": {"driver": DRIVER}}, {})), "one bad run among good"
print("ok")
