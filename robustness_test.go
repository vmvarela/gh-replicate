package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func fakeAPI(t *testing.T, handler http.HandlerFunc) *replicator {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	transport := transportFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	opts := api.ClientOptions{Host: "github.com", AuthToken: "test-token", Transport: transport, Timeout: time.Second}
	c, err := api.NewRESTClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHTTPClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &replicator{client: c, httpClient: h, ctx: context.Background(), timeout: time.Minute}
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
	labels, err := getAll[label](r, "first")
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
	if err := r.syncReleases("a/b", p); err != nil {
		t.Fatal(err)
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
	if err := r.syncMetadata("a/b", &plan{topics: topics, labels: []label{{Name: "new"}}}); err != nil {
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
	r.httpClient.Timeout = 20 * time.Millisecond
	if err := r.downloadAsset("https://api.github.com/slow", io.Discard); err == nil {
		t.Fatal("missing HTTP timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	if err := r.call("GET", "slow", nil, nil); err == nil {
		t.Fatal("missing REST cancellation")
	}
}

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
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
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := r.checkLFS(dir); err == nil || !strings.Contains(err.Error(), "Git LFS") {
		t.Fatal("missing LFS detection", err)
	}
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
	gitTest(t, src, "remote", "add", "replicate-target", dst)
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if err := r.pushRefs(src); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dst, "update-ref", "refs/heads/destination-only", first)
	gitTest(t, src, "commit", "--allow-empty", "-m", "second")
	second := gitTest(t, src, "rev-parse", "HEAD")
	gitTest(t, src, "branch", "-f", "a", second)
	gitTest(t, src, "branch", "-f", "b", second)
	r.dryRun = true
	if err := r.pushRefs(src); err != nil {
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
	if err := r.pushRefs(src); err == nil {
		t.Fatal("expected rejected push")
	}
	if got := gitTest(t, dst, "rev-parse", "a"); got != first {
		t.Fatal("partial push despite --atomic")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := r.pushRefs(src); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, dst, "rev-parse", "a"); got != second {
		t.Fatal("push did not update ref")
	}
	if got := gitTest(t, dst, "rev-parse", "destination-only"); got != first {
		t.Fatal("pruned destination-only ref")
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
	a := asset{Name: "binary", Size: 3, Digest: "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}
	if err := verifyAsset(f, a); err != nil {
		t.Fatal(err)
	}
	a.Size = 4
	if err := verifyAsset(f, a); err == nil {
		t.Fatal("accepted truncated download")
	}
}
