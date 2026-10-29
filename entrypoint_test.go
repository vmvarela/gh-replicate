package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sourceRepo = "alice/source"
	targetRepo = "bob/destination"
)

// seamStub pins the entrypoint seams for one test and restores them after.
func seamStub(t *testing.T) {
	t.Helper()
	isolateCache(t)
	oldSide, oldRemote := newSide, gitRemote
	t.Cleanup(func() { newSide, gitRemote = oldSide, oldRemote })
}

type gitFixture struct {
	source      string
	destination string
}

// newGitFixture builds an offline source (one commit, one tag) and an empty
// bare destination, both under t.TempDir().
func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, work, "init", "-b", "main")
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	gitTest(t, work, "commit", "--allow-empty", "-m", "first")
	gitTest(t, work, "tag", "v1")

	f := gitFixture{source: filepath.Join(root, "source.git"), destination: filepath.Join(root, "destination.git")}
	gitTest(t, root, "clone", "--bare", work, f.source)
	gitTest(t, root, "init", "--bare", f.destination)
	return f
}

// gitRefs returns git show-ref output, tolerating the "no refs" exit status.
func gitRefs(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "show-ref")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
			return ""
		}
		t.Fatalf("git show-ref: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// fakeGitHubAPI serves the endpoints preflight reads. When writable, the
// metadata writes of a real run are accepted and label PATCH returns 404 so
// syncMetadata takes the POST path.
func fakeGitHubAPI(t *testing.T, labelSet []label, writable bool, defaultBranch string, actionsEnabled bool) (string, *[]string, *string, *[]string) {
	t.Helper()
	var methods, actionBodies []string
	var metadataPatch string
	server := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method+" "+req.URL.Path)
		if req.Method != http.MethodGet {
			if req.Method == http.MethodPatch && strings.Count(req.URL.Path, "/") == 3 {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				metadataPatch = string(body)
			}
			if !writable {
				t.Errorf("unexpected write: %s %s", req.Method, req.URL.Path)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/actions/permissions") {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				actionBodies = append(actionBodies, string(body))
			}
			if req.Method == http.MethodPatch && strings.HasSuffix(req.URL.Path, "/labels/bug") {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprint(w, `{}`)
			return
		}
		switch {
		case req.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"user"}`)
		case strings.HasSuffix(req.URL.Path, "/actions/permissions"):
			fmt.Fprintf(w, `{"enabled":%t}`, actionsEnabled)
		case strings.HasSuffix(req.URL.Path, "/topics"):
			fmt.Fprint(w, `{"names":[]}`)
		case strings.HasSuffix(req.URL.Path, "/labels"):
			if err := json.NewEncoder(w).Encode(labelSet); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(req.URL.Path, "/releases/latest"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case strings.HasSuffix(req.URL.Path, "/releases"):
			fmt.Fprint(w, `[]`)
		case strings.HasPrefix(req.URL.Path, "/repos/"):
			payload := map[string]any{
				"id":             testRepoID(strings.TrimPrefix(req.URL.Path, "/repos/")),
				"description":    "replicated",
				"default_branch": defaultBranch,
			}
			if err := json.NewEncoder(w).Encode(payload); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})
	return server, &methods, &metadataPatch, &actionBodies
}

// stubRun wires newSide and gitRemote to the fake servers and local bare repos.
func stubRun(t *testing.T, sourceURL, targetURL string, f gitFixture) {
	t.Helper()
	seamStub(t)
	src := fakeSide(t, sourceURL, "source-token")
	dst := fakeSide(t, targetURL, "target-token")
	newSide = func(role, account string, timeout time.Duration) (side, error) {
		if role == "source" {
			return src, nil
		}
		return dst, nil
	}
	gitRemote = func(ownerRepo string) string {
		switch ownerRepo {
		case sourceRepo:
			return f.source
		case targetRepo:
			return f.destination
		}
		t.Errorf("unexpected repository %q", ownerRepo)
		return ""
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		host string
		want string
	}{
		{"no arguments", nil, "github.com", "usage: gh replicate"},
		{"one argument", []string{"owner/repo"}, "github.com", "usage: gh replicate"},
		{"extra component", []string{"owner/repo/extra", "owner/other"}, "github.com", "OWNER/REPO"},
		{"dot-dot component", []string{"owner/..", "owner/other"}, "github.com", "OWNER/REPO"},
		{"identical repositories", []string{"Owner/Repo", "owner/repo"}, "github.com", "must be different"},
		{"zero timeout", []string{"--timeout", "0s", "owner/repo", "owner/other"}, "github.com", "timeout must be positive"},
		{"enterprise host", []string{"owner/repo", "owner/other"}, "ghe.example.com", "only github.com is supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_HOST", tc.host)
			seamStub(t)
			sideCalls, remoteCalls := 0, 0
			newSide = func(role, account string, timeout time.Duration) (side, error) {
				sideCalls++
				return side{}, nil
			}
			gitRemote = func(ownerRepo string) string {
				remoteCalls++
				return ""
			}

			err := run(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if sideCalls != 0 || remoteCalls != 0 {
				t.Fatalf("invalid arguments touched the seams: newSide=%d gitRemote=%d", sideCalls, remoteCalls)
			}
		})
	}
}

func TestRunLeavesDestinationUntouchedWhenPreflightFails(t *testing.T) {
	t.Setenv("GH_HOST", "github.com")
	f := newGitFixture(t)
	before := gitRefs(t, f.destination)

	var targetMethods []string
	targetURL := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		targetMethods = append(targetMethods, req.Method)
		switch {
		case req.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"bob"}`)
		case strings.HasSuffix(req.URL.Path, "/actions/permissions"):
			fmt.Fprint(w, `{"enabled":true}`)
		default:
			fmt.Fprint(w, `{"description":"","homepage":""}`)
		}
	})
	sourceURL := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/user" {
			fmt.Fprint(w, `{"login":"alice"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	stubRun(t, sourceURL, targetURL, f)

	err := run([]string{sourceRepo, targetRepo})
	if err == nil {
		t.Fatal("run accepted a missing source repository")
	}
	if after := gitRefs(t, f.destination); after != before {
		t.Fatalf("destination refs changed:\nbefore %q\nafter  %q", before, after)
	}
	for _, method := range targetMethods {
		if method != http.MethodGet {
			t.Fatalf("destination received a mutating request: %s", method)
		}
	}
}

// testRepoID derives a stable ID from a repository name so that separate fake
// servers agree on identity, the way GitHub does.
func testRepoID(name string) int64 {
	h := fnv.New64a()
	if _, err := h.Write([]byte(name)); err != nil {
		panic(err)
	}
	return int64(h.Sum64() >> 1)
}

func TestRunRejectsUnknownRepositoryID(t *testing.T) {
	for _, tc := range []struct {
		name, missing string
		dryRun        bool
	}{
		{"source ID missing", sourceRepo, false},
		{"target ID missing", targetRepo, false},
		{"source ID missing in dry run", sourceRepo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_HOST", "github.com")
			f := newGitFixture(t)
			before := gitRefs(t, f.destination)

			var methods []string
			serverURL := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
				methods = append(methods, req.Method)
				switch {
				case req.URL.Path == "/user":
					fmt.Fprint(w, `{"login":"alice"}`)
				case req.URL.Path == "/repos/"+tc.missing:
					fmt.Fprint(w, `{"id":0,"description":"","default_branch":"main"}`)
				case strings.HasPrefix(req.URL.Path, "/repos/"):
					fmt.Fprint(w, `{"id":7691631,"description":"","default_branch":"main"}`)
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
			})
			stubRun(t, serverURL, serverURL, f)

			args := []string{sourceRepo, targetRepo}
			if tc.dryRun {
				args = append([]string{"--dry-run"}, args...)
			}
			err := run(args)
			if err == nil || !strings.Contains(err.Error(), "cannot read the repository IDs") {
				t.Fatalf("error = %v, want an abort for a missing repository ID", err)
			}
			if after := gitRefs(t, f.destination); after != before {
				t.Fatalf("destination refs changed:\nbefore %q\nafter  %q", before, after)
			}
			for _, method := range methods {
				if method != http.MethodGet {
					t.Fatalf("a mutating request escaped preflight: %s", method)
				}
			}
		})
	}
}

func TestRunRejectsOneRepositoryBehindTwoNames(t *testing.T) {
	t.Setenv("GH_HOST", "github.com")
	f := newGitFixture(t)
	before := gitRefs(t, f.destination)

	// GitHub answers both names with the same repository ID when the source was
	// renamed, which is exactly what a name-only check misses.
	var methods []string
	serverURL := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method)
		switch {
		case req.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"alice"}`)
		case strings.HasPrefix(req.URL.Path, "/repos/"):
			fmt.Fprint(w, `{"id":7691631,"description":"","default_branch":"main"}`)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})
	stubRun(t, serverURL, serverURL, f)

	err := run([]string{sourceRepo, targetRepo})
	if err == nil || !strings.Contains(err.Error(), "same repository") {
		t.Fatalf("error = %v, want an abort for one repository behind two names", err)
	}
	if after := gitRefs(t, f.destination); after != before {
		t.Fatalf("destination refs changed:\nbefore %q\nafter  %q", before, after)
	}
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("a mutating request escaped preflight: %s", method)
		}
	}
}

func TestRunDryRunLeavesDestinationUntouched(t *testing.T) {
	t.Setenv("GH_HOST", "github.com")
	f := newGitFixture(t)
	before := gitRefs(t, f.destination)

	labels := []label{{Name: "bug", Color: "d73a4a", Description: "Something isn't working"}}
	sourceURL, _, _, _ := fakeGitHubAPI(t, labels, false, "main", true)
	targetURL, targetMethods, _, actionBodies := fakeGitHubAPI(t, nil, false, "main", true)
	stubRun(t, sourceURL, targetURL, f)

	out := captureStderr(t, func() {
		if err := run([]string{"--dry-run", sourceRepo, targetRepo}); err != nil {
			t.Errorf("dry run failed: %v", err)
		}
	})
	if !strings.Contains(out, "Dry run complete") {
		t.Fatalf("missing dry-run confirmation: %q", out)
	}
	if !strings.Contains(out, "Would PUT repos/"+targetRepo+"/actions/permissions") {
		t.Fatalf("dry run did not report the Actions update: %q", out)
	}
	if len(*actionBodies) != 0 {
		t.Fatalf("dry run wrote Actions settings: %q", *actionBodies)
	}
	if after := gitRefs(t, f.destination); after != before {
		t.Fatalf("destination refs changed:\nbefore %q\nafter  %q", before, after)
	}
	for _, entry := range *targetMethods {
		if !strings.HasPrefix(entry, http.MethodGet+" ") {
			t.Fatalf("destination received a mutating request: %s", entry)
		}
	}
}

func TestRunReplicatesIntoDestination(t *testing.T) {
	t.Setenv("GH_HOST", "github.com")
	f := newGitFixture(t)

	labels := []label{{Name: "bug", Color: "d73a4a", Description: "Something isn't working"}}
	sourceURL, _, _, _ := fakeGitHubAPI(t, labels, false, "master", true)
	targetURL, targetMethods, targetPatch, actionBodies := fakeGitHubAPI(t, nil, true, "main", true)
	stubRun(t, sourceURL, targetURL, f)

	out := captureStderr(t, func() {
		if err := run([]string{sourceRepo, targetRepo}); err != nil {
			t.Errorf("replication failed: %v", err)
		}
	})
	if !strings.Contains(out, "Replication complete") {
		t.Fatalf("missing completion message: %q", out)
	}
	if got, want := gitRefs(t, f.destination), gitRefs(t, f.source); got != want {
		t.Fatalf("destination refs differ:\n got %q\nwant %q", got, want)
	}
	if len(*actionBodies) != 1 {
		t.Fatalf("Actions PUT bodies = %q, want one", *actionBodies)
	}
	var actionsPayload map[string]any
	if err := json.Unmarshal([]byte((*actionBodies)[0]), &actionsPayload); err != nil {
		t.Fatal(err)
	}
	if enabled, ok := actionsPayload["enabled"]; !ok || enabled != false {
		t.Fatalf("Actions payload = %s, want {\"enabled\":false}", (*actionBodies)[0])
	}
	issued := strings.Join(*targetMethods, "\n")
	for _, want := range []string{
		"PATCH /repos/" + targetRepo,
		"PUT /repos/" + targetRepo + "/topics",
		"POST /repos/" + targetRepo + "/labels",
	} {
		if !strings.Contains(issued, want) {
			t.Fatalf("missing metadata request %q in:\n%s", want, issued)
		}
	}
	var patch map[string]any
	if err := json.Unmarshal([]byte(*targetPatch), &patch); err != nil {
		t.Fatalf("decode metadata patch %q: %v", *targetPatch, err)
	}
	if patch["default_branch"] != "master" {
		t.Fatalf("default_branch = %v, want master from the source (destination had main)", patch["default_branch"])
	}
}

func TestSyncGitPropagatesRejectedPush(t *testing.T) {
	f := newGitFixture(t)
	hook := filepath.Join(f.destination, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	seamStub(t)
	gitRemote = func(ownerRepo string) string {
		switch ownerRepo {
		case sourceRepo:
			return f.source
		case targetRepo:
			return f.destination
		}
		t.Fatalf("unexpected repository %q", ownerRepo)
		return ""
	}
	side := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"enabled":false}`)
	}), "token")
	r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute}

	err := r.syncGit(sourceRepo, targetRepo)
	if err == nil || !strings.Contains(err.Error(), "push refs") {
		t.Fatalf("error = %v, want a rejected push", err)
	}
	if refs := gitRefs(t, f.destination); refs != "" {
		t.Fatalf("rejected push updated destination refs: %q", refs)
	}
}

func TestLFSSyncGitBlocksHistoricalPointerBeforeDestinationEffects(t *testing.T) {
	f := newGitFixture(t)
	work := filepath.Join(t.TempDir(), "w")
	gitTest(t, filepath.Dir(work), "clone", f.source, work)
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	pointer := "version https://hawser.github.com/spec/v1\r\noid sha256:" + strings.Repeat("a", 64) + "\r\nsize 12\r\n"
	pointer += strings.Repeat("\n", 2127-len(pointer)) // padded beyond the 1024-byte prefix
	if err := os.WriteFile(filepath.Join(work, "big.bin"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, work, "add", "big.bin")
	gitTest(t, work, "commit", "-m", "add pointer")
	gitTest(t, work, "rm", "big.bin")
	gitTest(t, work, "commit", "-m", "remove pointer")
	gitTest(t, work, "push", "origin", "main")

	seamStub(t)
	gitRemote = func(ownerRepo string) string {
		if ownerRepo == sourceRepo {
			return f.source
		}
		if ownerRepo == targetRepo {
			return f.destination
		}
		t.Fatalf("unexpected repository %q", ownerRepo)
		return ""
	}
	calls := 0
	side := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		fmt.Fprint(w, `{"enabled":false}`)
	}), "token")
	for _, dry := range []bool{false, true} {
		r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute, dryRun: dry}
		err := r.syncGit(sourceRepo, targetRepo)
		if err == nil || !strings.Contains(err.Error(), "Git LFS pointers detected") {
			t.Fatalf("dryRun=%v error = %v, want LFS detection", dry, err)
		}
		if refs := gitRefs(t, f.destination); refs != "" {
			t.Fatalf("dryRun=%v destination refs published: %q", dry, refs)
		}
		if calls != 0 {
			t.Fatalf("dryRun=%v API called %d times before LFS block", dry, calls)
		}
	}
}

func TestSyncGitClonesAndPushesRefs(t *testing.T) {
	f := newGitFixture(t)
	seamStub(t)
	gitRemote = func(ownerRepo string) string {
		switch ownerRepo {
		case sourceRepo:
			return f.source
		case targetRepo:
			return f.destination
		}
		t.Fatalf("unexpected repository %q", ownerRepo)
		return ""
	}
	side := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"enabled":false}`)
	}), "token")
	r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute}

	if err := r.syncGit(sourceRepo, targetRepo); err != nil {
		t.Fatal(err)
	}
	branch := gitTest(t, f.source, "symbolic-ref", "--short", "HEAD")
	if got, want := gitTest(t, f.destination, "rev-parse", branch), gitTest(t, f.source, "rev-parse", branch); got != want {
		t.Fatalf("branch %s = %s, want %s", branch, got, want)
	}
	if got, want := gitTest(t, f.destination, "rev-parse", "refs/tags/v1"), gitTest(t, f.source, "rev-parse", "refs/tags/v1"); got != want {
		t.Fatalf("tag v1 = %s, want %s", got, want)
	}
}

func TestSyncMetadataSetsDestinationDefaultBranch(t *testing.T) {
	var patch string
	server := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
			}
			patch = string(body)
		}
		fmt.Fprint(w, `{}`)
	})
	r := &replicator{ctx: context.Background(), target: fakeSide(t, server, "test-token")}
	p := &plan{repository: repository{Description: "d", DefaultBranch: "master"}}
	if err := r.syncMetadata(targetRepo, p); err != nil {
		t.Fatalf("sync metadata: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(patch), &body); err != nil {
		t.Fatalf("decode patch %q: %v", patch, err)
	}
	if body["default_branch"] != "master" {
		t.Fatalf("default_branch = %q, want master", body["default_branch"])
	}
}

// componentServer serves a source with releases v1 (also in the destination,
// so it is updated and Latest) and v2 (created), and records destination writes.
func componentServer(t *testing.T, isTarget bool, labelSet []label, writes *[]string) string {
	t.Helper()
	return fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.Method != http.MethodGet {
			*writes = append(*writes, req.Method+" "+req.URL.Path+" "+string(body))
			if req.Method == http.MethodPatch && strings.HasSuffix(req.URL.Path, "/labels/bug") {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprint(w, `{"id":12}`)
			return
		}
		p := req.URL.Path
		switch {
		case p == "/user":
			fmt.Fprint(w, `{"login":"user"}`)
		case strings.HasSuffix(p, "/actions/permissions"):
			fmt.Fprint(w, `{"enabled":true}`)
		case strings.HasSuffix(p, "/topics"):
			fmt.Fprint(w, `{"names":[]}`)
		case strings.HasSuffix(p, "/labels"):
			_ = json.NewEncoder(w).Encode(labelSet)
		case strings.HasSuffix(p, "/releases/latest"):
			fmt.Fprint(w, `{"id":1,"tag_name":"v1"}`)
		case strings.HasSuffix(p, "/releases") && isTarget:
			fmt.Fprint(w, `[{"id":11,"tag_name":"v1"}]`)
		case strings.HasSuffix(p, "/releases"):
			fmt.Fprint(w, `[{"id":1,"tag_name":"v1","name":"one"},{"id":2,"tag_name":"v2","name":"two"}]`)
		case strings.HasPrefix(p, "/repos/"):
			fmt.Fprintf(w, `{"id":%d,"description":"d","default_branch":"main"}`, testRepoID(strings.TrimPrefix(p, "/repos/")))
		default:
			t.Errorf("unexpected request: %s %s", req.Method, p)
		}
	})
}

func TestRunComponentsSelection(t *testing.T) {
	t.Setenv("GH_HOST", "github.com")
	names := []string{"refs", "metadata", "labels", "releases"}
	// Every non-empty subset (bitmask) plus the default (all four).
	var lists []string
	for mask := 1; mask < 16; mask++ {
		var sel []string
		for i, n := range names {
			if mask&(1<<i) != 0 {
				sel = append(sel, n)
			}
		}
		lists = append(lists, strings.Join(sel, ","))
	}
	lists = append(lists, "", "\x00empty") // flag omitted, flag given as ''
	for _, list := range lists {
		explicitEmpty := list == "\x00empty"
		if explicitEmpty {
			list = ""
		}
		has := func(n string) bool { return list == "" || strings.Contains(","+list+",", ","+n+",") }
		args := []string{}
		if list != "" || explicitEmpty {
			args = append(args, "--components", list)
		}
		args = append(args, sourceRepo, targetRepo)
		if !has("refs") && has("releases") {
			// Rejected before any I/O: no credentials resolved, no API call.
			seamStub(t)
			calls := 0
			newSide = func(string, string, time.Duration) (side, error) { calls++; return side{}, nil }
			if err := run(args); err == nil || !strings.Contains(err.Error(), "requires refs") || calls != 0 {
				t.Fatalf("%s: err=%v calls=%d, want rejection before I/O", list, err, calls)
			}
			continue
		}
		var sel, skip []string
		for _, n := range names {
			if has(n) {
				sel = append(sel, n)
			} else {
				skip = append(skip, n)
			}
		}
		skipped := "none"
		if len(skip) > 0 {
			skipped = strings.Join(skip, ",")
		}
		report := "Components selected: " + strings.Join(sel, ",") + "; skipped: " + skipped + "\n"

		for _, dry := range []bool{true, false} {
			f := newGitFixture(t)
			var srcWrites, dstWrites []string
			labels := []label{{Name: "bug", Color: "d73a4a"}}
			sourceURL := componentServer(t, false, labels, &srcWrites)
			targetURL := componentServer(t, true, nil, &dstWrites)
			stubRun(t, sourceURL, targetURL, f)
			a := args
			if dry {
				a = append([]string{"--dry-run"}, args...)
			}
			out := captureStderr(t, func() {
				if err := run(a); err != nil {
					t.Errorf("%s dry=%v: %v", list, dry, err)
				}
			})
			if !strings.Contains(out, report) {
				t.Fatalf("%s dry=%v: want %q in %q", list, dry, report, out)
			}
			if has("refs") != strings.Contains(out, "matching refs will be overwritten in the target") {
				t.Fatalf("%s dry=%v: overwrite warning mismatch: %q", list, dry, out)
			}
			if len(srcWrites) != 0 {
				t.Fatalf("%s: source written: %q", list, srcWrites)
			}
			pushed := gitRefs(t, f.destination) != ""
			if dry {
				if pushed || len(dstWrites) != 0 {
					t.Fatalf("%s: dry run wrote refs=%v requests=%q", list, pushed, dstWrites)
				}
				if has("releases") != strings.Contains(out, "Would POST repos/"+targetRepo+"/releases") {
					t.Fatalf("%s: dry-run release report mismatch: %q", list, out)
				}
				continue
			}
			all := strings.Join(dstWrites, "\n")
			effects := map[string]bool{
				"refs":     pushed,
				"metadata": strings.Contains(all, "PATCH /repos/"+targetRepo+" ") && strings.Contains(all, "PUT /repos/"+targetRepo+"/topics "),
				"labels":   strings.Contains(all, "POST /repos/"+targetRepo+"/labels "),
				"releases": strings.Contains(all, "POST /repos/"+targetRepo+"/releases {") && strings.Contains(all, `"tag_name":"v2"`) &&
					strings.Contains(all, "PATCH /repos/"+targetRepo+"/releases/11 {") && strings.Contains(all, `"make_latest":"true"`),
			}
			for _, n := range names {
				if effects[n] != has(n) {
					t.Fatalf("%s: effect %s = %v, want %v; writes:\n%s", list, n, effects[n], has(n), all)
				}
			}
			if has("refs") != strings.Contains(all, "PUT /repos/"+targetRepo+"/actions/permissions ") {
				t.Fatalf("%s: Actions disabling must happen exactly with refs:\n%s", list, all)
			}
			if has("refs") && gitRefs(t, f.destination) != gitRefs(t, f.source) {
				t.Fatalf("%s: refs differ", list)
			}
		}
	}
}
