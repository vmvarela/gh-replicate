"""Verify the real replica and stable asset IDs after a second run."""
import json
import os
import subprocess
import sys

GIT_AUTH = ["git", "-c", "credential.helper=", "-c", "credential.https://github.com.helper=!gh auth git-credential", "-c", "http.https://github.com/.extraheader="]

source = os.environ["SOURCE_REPOSITORY"]
target = os.environ["TARGET_REPOSITORY"]

def api(path):
    return json.loads(subprocess.check_output(["gh", "api", path], text=True))

def pages(path):
    result = []
    page = 1
    while True:
        batch = api(f"{path}?per_page=100&page={page}")
        result.extend(batch)
        if len(batch) < 100:
            return result
        page += 1

def releases(repo):
    result = {}
    for r in pages(f"repos/{repo}/releases"):
        r["assets"] = pages(f"repos/{repo}/releases/{r['id']}/assets")
        result[r["tag_name"]] = r
    return result

def ids(rs):
    return {tag: {a["name"]: a["id"] for a in r["assets"]} for tag, r in rs.items()}

def latest(repo):
    result = subprocess.run(["gh", "api", f"repos/{repo}/releases/latest"], capture_output=True, text=True)
    if result.returncode:
        if "HTTP 404" in result.stderr:
            return None
        raise RuntimeError(result.stderr)
    return json.loads(result.stdout)["tag_name"]

dst = releases(target)
if sys.argv[1] == "snapshot":
    print(json.dumps(ids(dst), sort_keys=True))
    sys.exit(0)

src = releases(source)
count = 0
for tag, r in src.items():
    assert tag in dst, f"Missing release {tag}"
    d = dst[tag]
    for key in ("name", "body", "draft", "prerelease"):
        assert r[key] == d[key], f"Release {tag}: {key} differs"
    assets = {a["name"]: a for a in d["assets"]}
    for a in r["assets"]:
        assert a["name"] in assets, f"Missing asset {tag}/{a['name']}"
        b = assets[a["name"]]
        assert a["size"] == b["size"], f"Asset size differs: {a['name']}"
        assert a.get("digest") and a["digest"] == b.get("digest"), f"Asset digest differs: {a['name']}"
        count += 1
with open(sys.argv[2]) as f:
    assert json.load(f) == ids(dst), "Second run recreated assets"

for key in ("description", "homepage"):
    assert api(f"repos/{source}")[key] == api(f"repos/{target}")[key], f"{key} differs"
assert set(api(f"repos/{source}/topics")["names"]) <= set(api(f"repos/{target}/topics")["names"])
labels = {x["name"]: x for x in pages(f"repos/{target}/labels")}
for label in pages(f"repos/{source}/labels"):
    assert label["name"] in labels
    for key in ("color", "description"):
        assert (label[key] or "") == (labels[label["name"]][key] or ""), f"Label {label['name']}: {key}: {label[key]!r} != {labels[label['name']][key]!r}"

def refs(repo):
    output = subprocess.check_output(GIT_AUTH + ["ls-remote", "--heads", "--tags", f"https://github.com/{repo}.git"], text=True)
    return {line.split()[1]: line.split()[0] for line in output.splitlines()}
target_refs = refs(target)
for name, sha in refs(source).items():
    assert target_refs.get(name) == sha, f"Ref mismatch: {name}"
source_latest = latest(source)
if source_latest is not None:
    assert latest(target) == source_latest, "Latest release differs"
print(f"Verified {len(src)} releases, {count} asset digests, refs, metadata, topics, labels, Latest and stable asset IDs.")

