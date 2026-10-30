package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func fakeServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func fakeSide(t *testing.T, server, token string) side {
	t.Helper()
	u, _ := url.Parse(server)
	transport := transportFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	opts := api.ClientOptions{Host: "github.com", AuthToken: token, Transport: transport, Timeout: time.Second}
	c, err := api.NewRESTClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHTTPClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return side{client: c, httpClient: h, token: token}
}

func fakeAPI(t *testing.T, handler http.HandlerFunc) *replicator {
	t.Helper()
	s := fakeSide(t, fakeServer(t, handler), "test-token")
	return &replicator{source: s, target: s, ctx: context.Background(), timeout: time.Minute, cacheDir: t.TempDir()}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	defer func() {
		os.Stderr = old
		writer.Close()
		reader.Close()
	}()
	fn()
	writer.Close()
	os.Stderr = old
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestPagination(t *testing.T) {
	calls := 0
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Link", `<https://api.github.com/next?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"name":"first"}]`)
		} else {
			if req.URL.Path != "/next" {
				t.Errorf("wrong next URL: %s", req.URL)
			}
			fmt.Fprint(w, `[{"name":"second"}]`)
		}
	})
	labels, err := getAll[label](r, r.source, "first")
	if err != nil || len(labels) != 2 || labels[1].Name != "second" || calls != 2 {
		t.Fatalf("pagination: %#v %v (%d calls)", labels, err, calls)
	}
}

func TestLatestExplicitAndLast(t *testing.T) {
	var requests []map[string]any
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body)
		if len(body) == 1 {
			if req.URL.Path != "/repos/a/b/releases/2" {
				t.Errorf("Latest must follow the source's choice: %s", req.URL)
			}
		} else if body["make_latest"] != "false" {
			t.Error("intermediate write promotes Latest")
		}
		fmt.Fprintf(w, `{"id":%d}`, len(requests))
	})
	p := &plan{sourceReleases: []release{{TagName: "v9"}, {TagName: "v1"}, {TagName: "v5"}}, latest: "v1"}
	if err := r.syncReleases("a/b", p); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || requests[3]["make_latest"] != "true" {
		t.Fatalf("wrong Latest requests: %#v", requests)
	}
}

func TestDryRunMakesNoWrites(t *testing.T) {
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		t.Errorf("unexpected network request: %s %s", req.Method, req.URL)
		fmt.Fprint(w, "{}")
	})
	r.dryRun = true
	p := &plan{topics: []string{"a"}, labels: []label{{Name: "bug"}}, sourceReleases: []release{{TagName: "v1", Assets: []asset{{Name: "binary"}}}}, latest: "v1"}
	if err := r.syncMetadata("a/b", p); err != nil {
		t.Fatal(err)
	}
	if err := r.syncLabels("a/b", p); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		if err := r.syncReleases("a/b", p); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "/releases/0") || !strings.Contains(out, "Latest to release v1") {
		t.Fatalf("misleading dry-run output: %q", out)
	}
}

func TestMetadataPreservesTopicsAndUpsertsLabels(t *testing.T) {
	topics, err := unionTopics([]string{"source", "shared"}, []string{"target", "shared"})
	if err != nil || !reflect.DeepEqual(topics, []string{"source", "shared", "target"}) {
		t.Fatal(topics, err)
	}
	var methods []string
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method+" "+req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/topics") {
			var body struct {
				Names []string `json:"names"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(body.Names, topics) {
				t.Error("lost destination topics")
			}
		}
		if req.Method == "PATCH" && strings.HasSuffix(req.URL.Path, "/labels/new") {
			w.WriteHeader(404)
		}
		fmt.Fprint(w, "{}")
	})
	p := &plan{topics: topics, labels: []label{{Name: "new"}}}
	if err := r.syncMetadata("a/b", p); err != nil {
		t.Fatal(err)
	}
	if err := r.syncLabels("a/b", p); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 4 || methods[3] != "POST /repos/a/b/labels" {
		t.Fatal(methods)
	}
}

func TestPreflightRejectsAmbiguousInput(t *testing.T) {
	names := make([]string, 21)
	for i := range names {
		names[i] = fmt.Sprint(i)
	}
	if _, err := unionTopics(names, nil); err == nil {
		t.Fatal("accepted more than 20 topics")
	}
	for _, releases := range [][]release{{{TagName: ""}}, {{TagName: "v1"}, {TagName: "v1", Draft: true}}} {
		if err := uniqueTags(releases); err == nil {
			t.Fatal("accepted ambiguous tags")
		}
	}
}

func TestRetryReadsOnly(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		calls := 0
		transport := retryTransport{base: transportFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"0"}}, Body: io.NopCloser(strings.NewReader("limited"))}, nil
		})}
		req, _ := http.NewRequest(method, "https://api.github.com/test", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := 1
		if method == "GET" {
			want = 3
		}
		if calls != want {
			t.Fatalf("%s retried %d times", method, calls)
		}
	}
	if _, retry := retryDelay(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}}, 0); retry {
		t.Fatal("must not retry before Retry-After")
	}
}

func TestHTTPTimeoutAndCancellation(t *testing.T) {
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
	r.target.httpClient.Timeout = 20 * time.Millisecond
	if err := r.downloadAsset(r.source, "https://api.github.com/slow", io.Discard); err == nil {
		t.Fatal("missing HTTP timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	if err := r.call(r.target, "GET", "slow", nil, nil); err == nil {
		t.Fatal("missing REST cancellation")
	}
}

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Detached auto-maintenance keeps writing into .git past the end of a test.
	cmd := exec.Command("git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLFSPointerDetection(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "--bare")
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if err := r.checkLFS(dir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 12345\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	// Unreachable cached pointers must not prevent replication.
	if err := r.checkLFS(dir); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "update-ref", "refs/tags/pointer", strings.TrimSpace(string(out)))
	if err := r.checkLFS(dir); err == nil || !strings.Contains(err.Error(), "Git LFS") {
		t.Fatal("missing LFS detection", err)
	}
}

func TestLFSPointerVariants(t *testing.T) {
	tail := "oid sha256:" + strings.Repeat("a", 64) + "\nsize 12\n"
	cases := map[string]struct {
		content string
		want    bool
	}{
		"lfs":            {"version https://git-lfs.github.com/spec/v1\n" + tail, true},
		"hawser":         {"version https://hawser.github.com/spec/v1\n" + tail, true},
		"git-media":      {"version http://git-media.io/v/2\n" + tail, true},
		"crlf":           {"version https://git-lfs.github.com/spec/v1\r\n" + strings.ReplaceAll(tail, "\n", "\r\n"), true},
		"whitespace":     {"  \n  version https://hawser.github.com/spec/v1\n" + tail, true},
		"extension":      {"ext-0-foo sha256:" + strings.Repeat("b", 64) + "\nversion https://git-lfs.github.com/spec/v1\n" + tail, true},
		"extension-crlf": {"ext-0-foo sha256:" + strings.Repeat("b", 64) + "\r\nversion https://git-lfs.github.com/spec/v1\r\n" + strings.ReplaceAll(tail, "\n", "\r\n"), true},
		"plain":          {"hello\n", false},
		"prose":          {"see version https://git-lfs.github.com/spec/v1 for details\n", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			gitTest(t, dir, "init", "--bare")
			cmd := exec.Command("git", "hash-object", "-w", "--stdin")
			cmd.Dir = dir
			cmd.Stdin = strings.NewReader(c.content)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			gitTest(t, dir, "update-ref", "refs/tags/p", strings.TrimSpace(string(out)))
			r := &replicator{ctx: context.Background(), timeout: time.Minute}
			err = r.checkLFS(dir)
			if c.want && (err == nil || !strings.Contains(err.Error(), "Git LFS pointers detected")) {
				t.Fatalf("error = %v, want LFS detection", err)
			}
			if !c.want && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func lfsWorkRepo(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "files")
	return dir
}

func paddedPointer(size int) []byte {
	p := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 12\n"
	return []byte(p + strings.Repeat("\n", size-len(p)))
}

func wantLFS(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "Git LFS pointers detected") {
		t.Fatalf("error = %v, want LFS detection", err)
	}
}

func TestLFSPaddedPointerSizes(t *testing.T) {
	for _, size := range []int{1023, 1024, 1025, 1027, 2127} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := lfsWorkRepo(t, map[string][]byte{"p.bin": paddedPointer(size)})
			wantLFS(t, (&replicator{ctx: context.Background(), timeout: time.Minute}).checkLFS(dir))
		})
	}
}

// A large binary blob without trailing LF must be drained exactly so the
// next object (a pointer) is still framed and inspected, loose or packed.
func TestLFSFramingAfterLargeBlob(t *testing.T) {
	big := make([]byte, 70000)
	for i := range big {
		big[i] = byte(i*7 + 1)
		if big[i] == '\n' {
			big[i] = 0
		}
	}
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	plain := lfsWorkRepo(t, map[string][]byte{"a.bin": big, "b.txt": []byte("hi")})
	if err := r.checkLFS(plain); err != nil {
		t.Fatal(err)
	}
	for _, packed := range []bool{false, true} {
		dir := lfsWorkRepo(t, map[string][]byte{"a.bin": big, "z.ptr": paddedPointer(200)})
		if packed {
			gitTest(t, dir, "repack", "-adq")
		}
		wantLFS(t, r.checkLFS(dir))
	}
}

func TestLFSIgnoresReplaceRefs(t *testing.T) {
	dir := lfsWorkRepo(t, map[string][]byte{"p.bin": paddedPointer(300)})
	pointer := gitTest(t, dir, "rev-parse", "HEAD:p.bin")
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("harmless\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "replace", pointer, strings.TrimSpace(string(out)))
	wantLFS(t, (&replicator{ctx: context.Background(), timeout: time.Minute}).checkLFS(dir))
}

func TestAtomicPushAndDryRun(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	gitTest(t, src, "init")
	gitTest(t, dst, "init", "--bare")
	gitTest(t, src, "config", "user.name", "Test")
	gitTest(t, src, "config", "user.email", "test@example.com")
	gitTest(t, src, "commit", "--allow-empty", "-m", "first")
	first := gitTest(t, src, "rev-parse", "HEAD")
	gitTest(t, src, "branch", "a")
	gitTest(t, src, "branch", "b")

	// The push publishes a fetched snapshot, mirroring syncGit's ephemeral bare.
	aux := newAux(t)
	gitTest(t, src, "--git-dir="+aux.Dir, "config", "remote.replicate-target.url", dst)
	syncAux := func() {
		t.Helper()
		gitTest(t, src, "--git-dir="+aux.Dir, "fetch", src, "refs/heads/*:refs/heads/*")
	}
	syncAux()

	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if err := r.pushRefs(aux, sourceRepo); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dst, "update-ref", "refs/heads/destination-only", first)
	gitTest(t, src, "commit", "--allow-empty", "-m", "second")
	second := gitTest(t, src, "rev-parse", "HEAD")
	gitTest(t, src, "branch", "-f", "a", second)
	gitTest(t, src, "branch", "-f", "b", second)
	syncAux()
	r.dryRun = true
	if err := r.pushRefs(aux, sourceRepo); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, dst, "rev-parse", "a"); got != first {
		t.Fatal("dry-run modified destination")
	}
	r.dryRun = false
	hook := filepath.Join(dst, "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n[ \"$1\" != refs/heads/b ]\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := r.pushRefs(aux, sourceRepo); err == nil {
		t.Fatal("expected rejected push")
	}
	if got := gitTest(t, dst, "rev-parse", "a"); got != first {
		t.Fatal("partial push despite --atomic")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := r.pushRefs(aux, sourceRepo); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, dst, "rev-parse", "a"); got != second {
		t.Fatal("push did not update ref")
	}
	if got := gitTest(t, dst, "rev-parse", "destination-only"); got != first {
		t.Fatal("pruned destination-only ref")
	}
}

func TestAtomicPushIgnoresInheritedConfig(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	gitTest(t, src, "init")
	gitTest(t, dst, "init", "--bare")
	gitTest(t, src, "config", "user.name", "Test")
	gitTest(t, src, "config", "user.email", "test@example.com")
	gitTest(t, src, "commit", "--allow-empty", "-m", "first")
	first := gitTest(t, src, "rev-parse", "HEAD")
	gitTest(t, src, "branch", "a")

	aux := newAux(t)
	gitTest(t, src, "--git-dir="+aux.Dir, "config", "remote.replicate-target.url", dst)
	gitTest(t, src, "--git-dir="+aux.Dir, "fetch", src, "refs/heads/*:refs/heads/*")

	// An inherited pushurl override must be ignored: the push has to go to the
	// configured destination, not wherever the parent environment points.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.replicate-target.pushurl")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(t.TempDir(), "wrong.git"))

	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if err := r.pushRefs(aux, sourceRepo); err != nil {
		t.Fatalf("isolated push failed: %v", err)
	}
	if got := gitTest(t, dst, "rev-parse", "a"); got != first {
		t.Fatalf("destination did not receive the snapshot: %q != %q", got, first)
	}
}

func TestCorruptDownloadPreservesExistingAsset(t *testing.T) {
	writes := 0
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			writes++
		}
		fmt.Fprint(w, "bad")
	})
	src := release{Assets: []asset{{Name: "binary", Size: 3, Digest: "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", URL: "https://api.github.com/asset"}}}
	dst := release{ID: 1, Assets: []asset{{ID: 2, Name: "binary", Size: 3, Digest: "sha256:old"}}}
	err := r.syncAssets("a/b", src, dst)
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected integrity error, got %v", err)
	}
	if writes != 0 {
		t.Fatalf("corrupt download caused %d writes", writes)
	}
}

func TestVerifyDownloadedAsset(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "asset")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("abc"); err != nil {
		t.Fatal(err)
	}
	a := asset{Name: "binary", Size: 3, Digest: assetDigest("abc")}
	digest, err := verifyAsset(f, a)
	if err != nil {
		t.Fatal(err)
	}
	if digest != a.Digest {
		t.Fatalf("digest %q, want %q", digest, a.Digest)
	}
	a.Size = 4
	if _, err := verifyAsset(f, a); err == nil {
		t.Fatal("accepted truncated download")
	}
}

func TestMissingDigestsPreserveAsset(t *testing.T) {
	var methods []string
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method+" "+req.URL.Path)
		switch req.URL.Path {
		case "/source", "/dest":
			fmt.Fprint(w, "abc")
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})
	src := release{Assets: []asset{{Name: "binary", Size: 3, URL: "https://api.github.com/source"}}}
	dst := release{ID: 1, Assets: []asset{{ID: 2, Name: "binary", Size: 3, URL: "https://api.github.com/dest"}}}
	if err := r.syncAssets("a/b", src, dst); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /source", "GET /dest"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("requests %v, want %v", methods, want)
	}
}

// inventory is a stateful destination: a failing attempt and the recovery
// attempt then operate on the assets the first one actually left behind.
type inventory struct {
	t          *testing.T
	assets     []*invAsset
	nextID     int64
	log        []string
	fail       map[string]string
	omitDigest bool
	source     string
	cacheDir   string
}

type invAsset struct {
	id    int64
	name  string
	body  string
	state string
}

func newInventory(t *testing.T, assets ...invAsset) *inventory {
	t.Helper()
	inv := &inventory{t: t, nextID: 10, fail: map[string]string{}, source: "abc", cacheDir: t.TempDir()}
	for _, a := range assets {
		a := a
		inv.assets = append(inv.assets, &a)
	}
	return inv
}

// replicator shares the inventory's cache directory, so a second run sees the
// journal the first one wrote.
func (inv *inventory) replicator() *replicator {
	r := fakeAPI(inv.t, inv.serve)
	r.cacheDir = inv.cacheDir
	return r
}

func (inv *inventory) find(name string) *invAsset {
	for _, a := range inv.assets {
		if a.name == name {
			return a
		}
	}
	return nil
}

func (inv *inventory) byID(id int64) *invAsset {
	for _, a := range inv.assets {
		if a.id == id {
			return a
		}
	}
	return nil
}

func (inv *inventory) body(name string) (string, bool) {
	a := inv.find(name)
	if a == nil {
		return "", false
	}
	return a.body, true
}

// take consumes a one-shot injected failure for the given transition.
func (inv *inventory) take(op string) string {
	mode := inv.fail[op]
	delete(inv.fail, op)
	return mode
}

func (inv *inventory) release() release {
	out := release{ID: 1}
	for _, a := range inv.assets {
		entry := asset{
			ID: a.id, Name: a.name, Size: int64(len(a.body)), State: a.state,
			URL: fmt.Sprintf("https://api.github.com/dest/%d", a.id),
		}
		if !inv.omitDigest {
			entry.Digest = assetDigest(a.body)
		}
		out.Assets = append(out.Assets, entry)
	}
	return out
}

func (inv *inventory) assetJSON(a *invAsset) map[string]any {
	entry := map[string]any{
		"id": a.id, "name": a.name, "size": len(a.body), "state": a.state,
		"url": fmt.Sprintf("https://api.github.com/dest/%d", a.id),
	}
	if !inv.omitDigest {
		entry["digest"] = assetDigest(a.body)
	}
	return entry
}

func (inv *inventory) serve(w http.ResponseWriter, req *http.Request) {
	inv.t.Helper()
	const assetsPath = "/repos/a/b/releases/1/assets"
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/source":
		fmt.Fprint(w, inv.source)
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/dest/"):
		inv.log = append(inv.log, "GET "+req.URL.Path)
		id, err := strconv.ParseInt(strings.TrimPrefix(req.URL.Path, "/dest/"), 10, 64)
		if err != nil {
			inv.t.Fatal(err)
		}
		a := inv.byID(id)
		if a == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, a.body)
	case req.Method == http.MethodGet && req.URL.Path == assetsPath:
		inv.log = append(inv.log, "GET assets")
		list := []map[string]any{}
		for _, a := range inv.assets {
			list = append(list, inv.assetJSON(a))
		}
		if err := json.NewEncoder(w).Encode(list); err != nil {
			inv.t.Fatal(err)
		}
	case req.Method == http.MethodPost && req.URL.Path == assetsPath:
		name := req.URL.Query().Get("name")
		inv.log = append(inv.log, "POST "+name)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			inv.t.Fatal(err)
		}
		mode := inv.take("upload")
		if mode == "fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inv.nextID++
		a := &invAsset{id: inv.nextID, name: name, body: string(body), state: "uploaded"}
		if mode == "starter" {
			a.body, a.state = "", "starter"
		}
		inv.assets = append(inv.assets, a)
		if mode == "lost" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if err := json.NewEncoder(w).Encode(inv.assetJSON(a)); err != nil {
			inv.t.Fatal(err)
		}
	case req.Method == http.MethodPatch && strings.HasPrefix(req.URL.Path, "/repos/a/b/releases/assets/"):
		id, err := strconv.ParseInt(path.Base(req.URL.Path), 10, 64)
		if err != nil {
			inv.t.Fatal(err)
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			inv.t.Fatal(err)
		}
		inv.log = append(inv.log, fmt.Sprintf("PATCH %d => %s", id, body.Name))
		mode := inv.take("rename")
		if mode == "fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		a := inv.byID(id)
		if a == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if other := inv.find(body.Name); other != nil && other.id != id {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"already_exists"}`)
			return
		}
		a.name = body.Name
		if mode == "lost" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "{}")
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/repos/a/b/releases/assets/"):
		id, err := strconv.ParseInt(path.Base(req.URL.Path), 10, 64)
		if err != nil {
			inv.t.Fatal(err)
		}
		inv.log = append(inv.log, fmt.Sprintf("DELETE %d", id))
		mode := inv.take("delete")
		if mode == "fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for i, a := range inv.assets {
			if a.id == id {
				inv.assets = append(inv.assets[:i], inv.assets[i+1:]...)
				break
			}
		}
		if mode == "lost" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "{}")
	default:
		inv.t.Errorf("unexpected request: %s %s", req.Method, req.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

const sourceBinary = "https://api.github.com/source"

func binarySource() release { return binarySourceBytes("abc") }

func binarySourceBytes(body string) release {
	return release{Assets: []asset{{Name: "binary", Size: int64(len(body)), Digest: assetDigest(body), URL: sourceBinary}}}
}

// requireOnlyAsset checks the release ended with exactly one asset of the given
// name and body.
func requireOnlyAsset(t *testing.T, inv *inventory, name, body string) {
	t.Helper()
	if len(inv.assets) != 1 {
		t.Fatalf("assets %+v, want only %q", inv.assets, name)
	}
	if got, ok := inv.body(name); !ok || got != body {
		t.Fatalf("asset %q body %q present=%v, want %q", name, got, ok, body)
	}
}

func TestReplacementStagesBeforeDeleting(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	r := inv.replicator()
	if err := r.syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	if len(inv.log) != 3 || !strings.HasPrefix(inv.log[0], "POST binary"+stagedSuffix) ||
		inv.log[1] != "DELETE 1" || !strings.HasSuffix(inv.log[2], "=> binary") {
		t.Fatalf("requests %v, want a staged upload, then the delete, then the rename", inv.log)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
	j, err := openJournal(inv.cacheDir, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.get(1, "binary"); ok {
		t.Fatal("journal entry survived a successful replacement")
	}
	j.Close()

	// The journal entry is gone, so a rerun writes nothing.
	inv.log = nil
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	if len(inv.log) != 0 {
		t.Fatalf("rerun wrote again: %v", inv.log)
	}
}

func TestFailedUploadKeepsPreviousAsset(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["upload"] = "fail"
	err := inv.replicator().syncAssets("a/b", binarySource(), inv.release())
	if err == nil || !strings.Contains(err.Error(), "upload outcome is unknown") {
		t.Fatalf("expected an unresolved upload error, got %v", err)
	}
	if body, ok := inv.body("binary"); !ok || body != "xyz" {
		t.Fatalf("previous asset body %q present=%v, want it preserved", body, ok)
	}
	if len(inv.assets) != 1 {
		t.Fatalf("assets %+v, want only the previous asset", inv.assets)
	}
}

// A starter left by an interrupted upload is not the only copy, but the tool
// no longer deletes it: an orphan is safer than a wrong deletion.
func TestStarterUploadIsLeftAndRerunSucceeds(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["upload"] = "starter"
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err == nil {
		t.Fatal("expected the interrupted upload to fail")
	}
	if len(inv.assets) != 2 || inv.assets[1].state != "starter" {
		t.Fatalf("assets %+v, want the previous one plus an empty starter", inv.assets)
	}

	// The second run must never delete the only full copy before the new one is
	// verified; the starter simply stays behind.
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	if body, ok := inv.body("binary"); !ok || body != "abc" {
		t.Fatalf("canonical body %q present=%v, want the new bytes", body, ok)
	}
	var starters int
	for _, a := range inv.assets {
		if a.state == "starter" {
			starters++
		}
	}
	if len(inv.assets) != 2 || starters != 1 {
		t.Fatalf("assets %+v, want the canonical plus the orphaned starter", inv.assets)
	}
}

func TestLostUploadResponseIsReconciled(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["upload"] = "lost"
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
}

func TestRenameFailureRecoversOnRerun(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["rename"] = "fail"
	err := inv.replicator().syncAssets("a/b", binarySource(), inv.release())
	if err == nil || !strings.Contains(err.Error(), "rename failed") {
		t.Fatalf("expected a rename error, got %v", err)
	}
	if len(inv.assets) != 1 || inv.assets[0].body != "abc" || !strings.HasPrefix(inv.assets[0].name, "binary"+stagedSuffix) {
		t.Fatalf("assets %+v, want only the verified staged copy", inv.assets)
	}

	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
}

// When the source asset changes between runs, the staged copy no longer
// verifies against the new digest. It may be the only full copy, so it stays.
func TestStagedCopyIsKeptWhenTheSourceChanges(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["rename"] = "fail"
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err == nil {
		t.Fatal("expected the rename to fail")
	}
	if len(inv.assets) != 1 || inv.assets[0].body != "abc" {
		t.Fatalf("assets %+v, want only the staged copy", inv.assets)
	}

	inv.source = "abd"
	if err := inv.replicator().syncAssets("a/b", binarySourceBytes("abd"), inv.release()); err != nil {
		t.Fatal(err)
	}
	if body, ok := inv.body("binary"); !ok || body != "abd" {
		t.Fatalf("canonical body %q present=%v, want the new bytes", body, ok)
	}
	var kept int
	for _, a := range inv.assets {
		if a.body == "abc" && strings.HasPrefix(a.name, "binary"+stagedSuffix) {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("assets %+v, want the earlier staged copy preserved", inv.assets)
	}
}

func TestRenameAppliedWithLostResponse(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["rename"] = "lost"
	err := inv.replicator().syncAssets("a/b", binarySource(), inv.release())
	if err == nil || !strings.Contains(err.Error(), "may have been applied") {
		t.Fatalf("expected an ambiguous rename error, got %v", err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")

	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
}

func TestDeleteFailureRecoversOnRerun(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["delete"] = "fail"
	err := inv.replicator().syncAssets("a/b", binarySource(), inv.release())
	if err == nil || !strings.Contains(err.Error(), "may or may not still exist") {
		t.Fatalf("expected a delete error, got %v", err)
	}
	if body, ok := inv.body("binary"); !ok || body != "xyz" {
		t.Fatalf("previous asset lost: %q present=%v", body, ok)
	}

	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
}

func TestDeleteAppliedWithLostResponse(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.fail["delete"] = "lost"
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err == nil {
		t.Fatal("expected the lost delete response to fail the run")
	}
	if len(inv.assets) != 1 || inv.assets[0].body != "abc" {
		t.Fatalf("assets %+v, want only the staged copy", inv.assets)
	}

	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
}

func TestNoDigestUploadIsVerifiedByDownload(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	inv.omitDigest = true
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	requireOnlyAsset(t, inv, "binary", "abc")
}

func TestUnjournaledStagedLookingAssetIsNotTouched(t *testing.T) {
	foreign := "binary" + stagedSuffix + "deadbeef"
	inv := newInventory(t,
		invAsset{id: 1, name: "binary", body: "xyz"},
		invAsset{id: 2, name: foreign, body: "abc"},
	)
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	if body, ok := inv.body(foreign); !ok || body != "abc" {
		t.Fatalf("staged-looking asset body %q present=%v, want it untouched", body, ok)
	}
	if body, ok := inv.body("binary"); !ok || body != "abc" {
		t.Fatalf("canonical body %q present=%v, want the new bytes", body, ok)
	}
}

func TestEmptyUserAssetIsNotDeleted(t *testing.T) {
	inv := newInventory(t,
		invAsset{id: 1, name: "binary", body: "xyz"},
		invAsset{id: 2, name: "notes", state: "uploaded"},
	)
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	if _, ok := inv.body("notes"); !ok {
		t.Fatal("empty destination-only asset was deleted")
	}
}

func TestDestinationOnlyAssetSurvivesReplacement(t *testing.T) {
	inv := newInventory(t,
		invAsset{id: 1, name: "binary", body: "xyz"},
		invAsset{id: 2, name: "user-file", body: "keep"},
	)
	if err := inv.replicator().syncAssets("a/b", binarySource(), inv.release()); err != nil {
		t.Fatal(err)
	}
	if body, ok := inv.body("user-file"); !ok || body != "keep" {
		t.Fatalf("destination-only asset body %q present=%v, want it preserved", body, ok)
	}
}

// A journal that cannot be parsed must stop the run rather than guess which
// staged assets are ours.
func TestCorruptJournalStopsReplacement(t *testing.T) {
	inv := newInventory(t, invAsset{id: 1, name: "binary", body: "xyz"})
	dir := filepath.Join(inv.cacheDir, "gh-replicate", "staged-v1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%x.json", sha256.Sum256([]byte("a/b")))
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	err := inv.replicator().syncAssets("a/b", binarySource(), inv.release())
	if err == nil || !strings.Contains(err.Error(), "parse journal") {
		t.Fatalf("expected a journal parse error, got %v", err)
	}
	if body, ok := inv.body("binary"); !ok || body != "xyz" {
		t.Fatalf("previous asset body %q present=%v, want it untouched", body, ok)
	}
	if len(inv.log) != 0 {
		t.Fatalf("wrote before failing: %v", inv.log)
	}
}

func TestDestinationDigestSkipsDownload(t *testing.T) {
	calls := 0
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.URL.Path != "/source" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		fmt.Fprint(w, "abc")
	})
	src := release{Assets: []asset{{Name: "binary", Size: 3, URL: "https://api.github.com/source"}}}
	dst := release{ID: 1, Assets: []asset{{ID: 2, Name: "binary", Size: 3, Digest: assetDigest("abc"), URL: "https://api.github.com/dest"}}}
	if err := r.syncAssets("a/b", src, dst); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("destination was downloaded %d times", calls-1)
	}
}

func TestDestinationReadErrorPreservesAsset(t *testing.T) {
	writes := 0
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/source":
			fmt.Fprint(w, "abc")
		case req.Method == http.MethodGet && req.URL.Path == "/dest":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			writes++
		}
	})
	src := release{Assets: []asset{{Name: "binary", Size: 3, URL: "https://api.github.com/source"}}}
	dst := release{ID: 1, Assets: []asset{{ID: 2, Name: "binary", Size: 3, URL: "https://api.github.com/dest"}}}
	err := r.syncAssets("a/b", src, dst)
	if err == nil || !strings.Contains(err.Error(), "compare asset") {
		t.Fatalf("expected comparison error, got %v", err)
	}
	if writes != 0 {
		t.Fatalf("failed comparison caused %d writes", writes)
	}
}
