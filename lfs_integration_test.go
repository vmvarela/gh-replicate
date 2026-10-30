//go:build lfsintegration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// lfsServer is a minimal Git LFS batch API + basic transfer over HTTP, enough
// for transferLFS to run against a real git-lfs without GitHub or credentials.
type lfsServer struct {
	mu          sync.Mutex
	objects     map[string][]byte
	actionToken string // short-lived transfer capability the batch issues
	failUpload  string // OID whose PUT fails (partial-upload test)
	puts        int    // PUTs received, proving a transfer was attempted
	requests    int    // every LFS request (batch + objects), proving no contact
}

func newLFSServer(t *testing.T) (*httptest.Server, *lfsServer) {
	t.Helper()
	s := &lfsServer{objects: map[string][]byte{}, actionToken: "test"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/info/lfs/objects/batch"):
			s.batch(w, r)
		case strings.HasPrefix(r.URL.Path, "/objects/"):
			s.object(w, r)
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, s
}

func (s *lfsServer) batch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	action := map[string]bool{"download": true, "upload": true}[req.Operation]
	if !action {
		http.Error(w, "unsupported operation", 400)
		return
	}
	objects := make([]map[string]any, 0, len(req.Objects))
	for _, o := range req.Objects {
		if req.Operation == "download" {
			if _, ok := s.objects[o.OID]; !ok {
				http.Error(w, "missing object "+o.OID, 404)
				return
			}
		}
		objects = append(objects, map[string]any{
			"oid":  o.OID,
			"size": o.Size,
			"actions": map[string]any{
				req.Operation: map[string]any{
					"href":   "http://" + r.Host + "/objects/" + o.OID,
					"header": map[string]string{"Authorization": "Bearer " + s.actionToken},
				},
			},
		})
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": objects})
}

func (s *lfsServer) object(w http.ResponseWriter, r *http.Request) {
	oid := strings.TrimPrefix(r.URL.Path, "/objects/")
	s.mu.Lock()
	defer s.mu.Unlock()
	// The action's header is a short-lived capability: it must reach the
	// transfer, so a missing or wrong one proves it was not relayed.
	if got := r.Header.Get("Authorization"); got != "Bearer "+s.actionToken {
		http.Error(w, "missing or wrong transfer authorization", 401)
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, ok := s.objects[oid]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		s.puts++
		if oid == s.failUpload {
			// Fail mid-transfer with a Git-LFS JSON error (not text/plain, which
			// git-lfs treats as a non-fatal "unsupported Content-Type").
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"injected upload failure"}`))
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.objects[oid] = data
		w.WriteHeader(200)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *lfsServer) add(oid string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[oid] = data
}

func (s *lfsServer) has(oid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[oid]
	return ok
}

func (s *lfsServer) get(oid string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[oid]
}

func (s *lfsServer) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func (s *lfsServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func canonicalPointer(oid string, size int64) string {
	return "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize " + strconv.FormatInt(size, 10) + "\n"
}

// TestLFSIntegrationTransfer runs the real transferLFS path over HTTP against a
// real git-lfs: a source bare with historical and tagged canonical pointers, an
// isolated source LFS server, and a fresh destination server. It requires
// git-lfs (>= 3.8.1) on PATH and fails otherwise.
func TestLFSIntegrationTransfer(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Fatalf("git-lfs is required for -tags=lfsintegration: %v", err)
	}
	content1 := []byte("hello lfs v1\n")
	content2 := []byte("hello lfs v2 longer\n")
	oid1, oid2 := sha256hex(content1), sha256hex(content2)

	src, srcStore := newLFSServer(t)
	dst, dstStore := newLFSServer(t)
	srcStore.add(oid1, content1)
	srcStore.add(oid2, content2)

	// Source bare with a branch (two pointer versions, one historical) and a tag.
	srcBare := filepath.Join(t.TempDir(), "src.git")
	gitTest(t, t.TempDir(), "init", "--bare", srcBare)
	work := t.TempDir()
	gitTest(t, work, "init", "-b", "main")
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid1, int64(len(content1)))), 0o644))
	gitTest(t, work, "add", ".")
	gitTest(t, work, "commit", "-m", "v1")
	gitTest(t, work, "tag", "v1")
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid2, int64(len(content2)))), 0o644))
	gitTest(t, work, "add", ".")
	gitTest(t, work, "commit", "-m", "v2")
	gitTest(t, work, "push", "file://"+srcBare, "refs/heads/main:refs/heads/main", "refs/tags/v1:refs/tags/v1")

	// Route the LFS endpoint seam to the synthetic servers.
	prev := gitRemote
	gitRemote = func(ownerRepo string) string {
		if ownerRepo == "dst" {
			return dst.URL
		}
		return src.URL
	}
	defer func() { gitRemote = prev }()

	a := newAux(t)
	r := &replicator{ctx: context.Background(), timeout: time.Minute, source: side{}, target: side{}}

	// The target remote must exist for `git lfs push` to name it; lfs.pushurl
	// (pinned to the synthetic server) drives the actual endpoint.
	must(t, a.run(context.Background(), "--git-dir="+a.Dir, "config", "remote.replicate-target.url", dst.URL))

	// Fetch source refs into the aux bare over file:// (no LFS here).
	must(t, a.run(context.Background(), "--git-dir="+a.Dir, "fetch", "--prune", "--no-tags",
		"file://"+srcBare, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"))
	refs, err := r.captureRefs(a.Dir)
	must(t, err)
	inv, err := r.inventoryLFS(a.Dir, refs)
	must(t, err)
	if len(inv) != 2 {
		t.Fatalf("inventory has %d objects, want 2", len(inv))
	}
	must(t, a.pointHEADAbsent(context.Background()))

	must(t, r.transferLFS(context.Background(), a, "src", "dst", refs, inv))

	for _, oid := range []string{oid1, oid2} {
		if !dstStore.has(oid) {
			t.Fatalf("destination missing LFS object %s", oid)
		}
	}
	if !bytes.Equal(dstStore.get(oid1), content1) {
		t.Fatalf("destination object %s bytes differ", oid1)
	}
	if !bytes.Equal(dstStore.get(oid2), content2) {
		t.Fatalf("destination object %s bytes differ", oid2)
	}
}

// TestLFSIntegrationPartialUploadFails ensures a mid-batch upload failure is
// reported as an error. syncGit returns on that error before disableActions and
// pushRefs, so a partial upload can never lead to publishing refs.
func TestLFSIntegrationPartialUploadFails(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Fatalf("git-lfs is required for -tags=lfsintegration: %v", err)
	}
	content1 := []byte("one\n")
	content2 := []byte("two\n")
	oid1, oid2 := sha256hex(content1), sha256hex(content2)

	src, srcStore := newLFSServer(t)
	dst, dstStore := newLFSServer(t)
	srcStore.add(oid1, content1)
	srcStore.add(oid2, content2)
	dstStore.failUpload = oid2

	srcBare := filepath.Join(t.TempDir(), "src.git")
	gitTest(t, t.TempDir(), "init", "--bare", srcBare)
	work := t.TempDir()
	gitTest(t, work, "init", "-b", "main")
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid1, int64(len(content1)))), 0o644))
	gitTest(t, work, "add", ".")
	gitTest(t, work, "commit", "-m", "one")
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid2, int64(len(content2)))), 0o644))
	gitTest(t, work, "add", ".")
	gitTest(t, work, "commit", "-m", "two")
	gitTest(t, work, "push", "file://"+srcBare, "refs/heads/main:refs/heads/main")

	prev := gitRemote
	gitRemote = func(ownerRepo string) string {
		if ownerRepo == "dst" {
			return dst.URL
		}
		return src.URL
	}
	defer func() { gitRemote = prev }()

	a := newAux(t)
	r := &replicator{ctx: context.Background(), timeout: time.Minute, source: side{}, target: side{}}
	must(t, a.run(context.Background(), "--git-dir="+a.Dir, "config", "remote.replicate-target.url", dst.URL))
	must(t, a.run(context.Background(), "--git-dir="+a.Dir, "fetch", "--prune", "--no-tags",
		"file://"+srcBare, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"))
	refs, err := r.captureRefs(a.Dir)
	must(t, err)
	inv, err := r.inventoryLFS(a.Dir, refs)
	must(t, err)
	if len(inv) != 2 {
		t.Fatalf("inventory has %d objects, want 2", len(inv))
	}
	must(t, a.pointHEADAbsent(context.Background()))

	err = r.transferLFS(context.Background(), a, "src", "dst", refs, inv)
	if err == nil || !strings.Contains(err.Error(), "upload Git LFS objects") {
		t.Fatalf("error = %v, want upload failure", err)
	}
	if dstStore.putCount() == 0 {
		t.Fatal("destination received no upload attempt before the failure")
	}
	if !dstStore.has(oid1) {
		t.Fatal("valid object was not stored before the injected failure")
	}
	if dstStore.has(oid2) {
		t.Fatal("failed object was stored despite the injected failure")
	}
}

// TestLFSIntegrationSyncGitDryRunTransfersNothing runs syncGit in dry-run over a
// source with a canonical pointer (its object absent) and asserts the report is
// emitted while the destination stays empty: no LFS transfer, no ref push.
func TestLFSIntegrationSyncGitDryRunTransfersNothing(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Fatalf("git-lfs is required for -tags=lfsintegration: %v", err)
	}
	f := newGitFixture(t)
	work := filepath.Join(t.TempDir(), "w")
	gitTest(t, filepath.Dir(work), "clone", f.source, work)
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	oid := strings.Repeat("a", 64)
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid, 12)), 0o644))
	gitTest(t, work, "add", "a.bin")
	gitTest(t, work, "commit", "-m", "add pointer")
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
	side := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"enabled":false}`)
	}), "token")
	r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute, dryRun: true}

	out := captureStderr(t, func() {
		if err := r.syncGit(sourceRepo, targetRepo); err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
	})
	if !strings.Contains(out, "Would transfer 1 Git LFS objects") {
		t.Fatalf("dry run did not report the LFS transfer: %q", out)
	}
	if refs := gitRefs(t, f.destination); refs != "" {
		t.Fatalf("dry run published destination refs: %q", refs)
	}
}

// TestLFSIntegrationSyncGitDownloadFailureStopsBeforePublishing runs syncGit over
// a source whose pointer names an absent object, so the LFS download fails; it
// asserts the run aborts before touching Actions or publishing any ref.
func TestLFSIntegrationSyncGitDownloadFailureStopsBeforePublishing(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Fatalf("git-lfs is required for -tags=lfsintegration: %v", err)
	}
	f := newGitFixture(t)
	work := filepath.Join(t.TempDir(), "w")
	gitTest(t, filepath.Dir(work), "clone", f.source, work)
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	oid := strings.Repeat("a", 64) // object deliberately absent from the source
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid, 12)), 0o644))
	gitTest(t, work, "add", "a.bin")
	gitTest(t, work, "commit", "-m", "add pointer")
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
	r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute}

	err := r.syncGit(sourceRepo, targetRepo)
	if err == nil || !strings.Contains(err.Error(), "download Git LFS objects") {
		t.Fatalf("error = %v, want LFS download failure", err)
	}
	if refs := gitRefs(t, f.destination); refs != "" {
		t.Fatalf("destination refs published despite transfer failure: %q", refs)
	}
	if calls != 0 {
		t.Fatalf("Actions API called %d times despite transfer failure", calls)
	}
}

// TestLFSIntegrationSyncGitDryRunMakesNoLFSRequests pins the LFS endpoint to a
// synthetic server (independent of the file:// git remote) and asserts a dry run
// never contacts it: transferLFS is skipped, so no LFS batch or object request.
func TestLFSIntegrationSyncGitDryRunMakesNoLFSRequests(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Fatalf("git-lfs is required for -tags=lfsintegration: %v", err)
	}
	f := newGitFixture(t)
	work := filepath.Join(t.TempDir(), "w")
	gitTest(t, filepath.Dir(work), "clone", f.source, work)
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	content := []byte("dry\n")
	oid := sha256hex(content)
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid, int64(len(content)))), 0o644))
	gitTest(t, work, "add", "a.bin")
	gitTest(t, work, "commit", "-m", "add pointer")
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
	lfsSrv, lfsStore := newLFSServer(t)
	lfsStore.add(oid, content)
	lfsEndpoint = func(ownerRepo string) string { return lfsSrv.URL + "/info/lfs" }

	side := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"enabled":false}`)
	}), "token")
	r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute, dryRun: true}

	out := captureStderr(t, func() {
		if err := r.syncGit(sourceRepo, targetRepo); err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
	})
	if !strings.Contains(out, "Would transfer 1 Git LFS objects") {
		t.Fatalf("dry run did not report the LFS transfer: %q", out)
	}
	if refs := gitRefs(t, f.destination); refs != "" {
		t.Fatalf("dry run published destination refs: %q", refs)
	}
	if n := lfsStore.requestCount(); n != 0 {
		t.Fatalf("dry run made %d LFS requests, want 0", n)
	}
}

// TestLFSIntegrationSyncGitUploadFailureStopsBeforePublishing runs syncGit with a
// source object that downloads fine and a destination that rejects its upload, so
// the failure happens after a real download; it asserts the run aborts before
// Actions or any ref push.
func TestLFSIntegrationSyncGitUploadFailureStopsBeforePublishing(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Fatalf("git-lfs is required for -tags=lfsintegration: %v", err)
	}
	f := newGitFixture(t)
	work := filepath.Join(t.TempDir(), "w")
	gitTest(t, filepath.Dir(work), "clone", f.source, work)
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	content := []byte("payload\n")
	oid := sha256hex(content)
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(canonicalPointer(oid, int64(len(content)))), 0o644))
	gitTest(t, work, "add", "a.bin")
	gitTest(t, work, "commit", "-m", "add pointer")
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
	srcSrv, srcStore := newLFSServer(t)
	dstSrv, dstStore := newLFSServer(t)
	srcStore.add(oid, content)
	dstStore.failUpload = oid
	lfsEndpoint = func(ownerRepo string) string {
		if ownerRepo == sourceRepo {
			return srcSrv.URL + "/info/lfs"
		}
		if ownerRepo == targetRepo {
			return dstSrv.URL + "/info/lfs"
		}
		t.Fatalf("unexpected repository %q", ownerRepo)
		return ""
	}

	calls := 0
	side := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		fmt.Fprint(w, `{"enabled":false}`)
	}), "token")
	r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute}

	err := r.syncGit(sourceRepo, targetRepo)
	if err == nil || !strings.Contains(err.Error(), "upload Git LFS objects") {
		t.Fatalf("error = %v, want LFS upload failure", err)
	}
	if refs := gitRefs(t, f.destination); refs != "" {
		t.Fatalf("destination refs published despite upload failure: %q", refs)
	}
	if calls != 0 {
		t.Fatalf("Actions API called %d times despite upload failure", calls)
	}
	if dstStore.has(oid) {
		t.Fatal("failed object stored despite injected upload failure")
	}
}
