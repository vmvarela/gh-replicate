#!/usr/bin/env python3
"""Allow a gosec SARIF upload only for a complete analysis.

Usage: check-security-sarif.py GOSEC_JSON GOSEC_SARIF
Exits non-zero (no upload) if either report is missing, empty or corrupt,
or if gosec reported Golang errors. Findings alone do not block the upload.
"""
import json
import sys


def check(json_path, sarif_path):
    try:
        with open(json_path) as f:
            report = json.load(f)
    except (OSError, ValueError) as e:
        return f"gosec JSON report is missing or corrupt: {e}"
    if not isinstance(report, dict) or "Golang errors" not in report:
        return "gosec JSON report has no 'Golang errors' field"
    if not isinstance(report["Golang errors"], dict):
        return "gosec JSON 'Golang errors' is not an object"
    if report["Golang errors"]:
        return f"gosec could not analyze every package: {json.dumps(report['Golang errors'])}"
    try:
        with open(sarif_path) as f:
            sarif = json.load(f)
    except (OSError, ValueError) as e:
        return f"gosec SARIF is missing, empty or corrupt: {e}"
    if not isinstance(sarif, dict) or sarif.get("version") != "2.1.0":
        return "gosec SARIF is not version 2.1.0"
    runs = sarif.get("runs")
    if not isinstance(runs, list) or not runs:
        return "gosec SARIF has no runs"
    for run in runs:
        if not isinstance(run, dict):
            return "gosec SARIF has an invalid run"
        driver = (run.get("tool") or {}).get("driver") if isinstance(run.get("tool"), dict) else None
        if not isinstance(driver, dict) or driver.get("name") != "gosec":
            return "gosec SARIF run is not from gosec"
        results = run.get("results")
        if not isinstance(results, list) or not all(isinstance(r, dict) for r in results):
            return "gosec SARIF run has invalid results"
    return None


if __name__ == "__main__":
    error = check(sys.argv[1], sys.argv[2])
    if error:
        raise SystemExit(error)
