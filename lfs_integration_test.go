//go:build lfsintegration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lfsServer is a minimal Git LFS batch API + basic transfer over HTTP, enough
// for transferLFS to run against a real git-lfs without GitHub or credentials.
type lfsServer struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newLFSServer(t *testing.T) (*httptest.Server, *lfsServer) {
	t.Helper()
	s := &lfsServer{objects: map[string][]byte{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					"header": map[string]string{"Authorization": "Bearer test"},
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
	switch r.Method {
	case http.MethodGet:
		data, ok := s.objects[oid]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
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

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(lfsCanonicalPointer(oid1, int64(len(content1)))), 0o644))
	gitTest(t, work, "add", ".")
	gitTest(t, work, "commit", "-m", "v1")
	gitTest(t, work, "tag", "v1")
	must(t, os.WriteFile(filepath.Join(work, "a.bin"), []byte(lfsCanonicalPointer(oid2, int64(len(content2)))), 0o644))
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
	must(t, a.pointHEADAbsent(context.Background(), refs))

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
