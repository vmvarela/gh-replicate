package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolateCache(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LOCALAPPDATA", root)
	// macOS derives its cache path from HOME.
	t.Setenv("HOME", root)
}

func TestCacheLockReleasedAfterProcessDeath(t *testing.T) {
	if path := os.Getenv("GH_REPLICATE_TEST_LOCK"); path != "" {
		f, err := lockCache(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Println("locked")
		time.Sleep(time.Minute)
		return
	}
	path := filepath.Join(t.TempDir(), "cache.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCacheLockReleasedAfterProcessDeath$")
	child.Env = append(os.Environ(), "GH_REPLICATE_TEST_LOCK="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("child readiness: %q, %v", line, err)
	}
	if f, err := lockCache(path); err == nil {
		f.Close()
		t.Fatal("concurrent lock acquired")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	f, err := lockCache(path)
	if err != nil {
		t.Fatalf("lock survived process death: %v", err)
	}
	f.Close()
}

func TestSyncGitCacheLifecycle(t *testing.T) {
	f := newGitFixture(t)
	seamStub(t)
	gitRemote = func(repo string) string {
		if repo == sourceRepo {
			return f.source
		}
		return f.destination
	}
	s := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, `{"enabled":false}`) }), "token")
	r := &replicator{source: s, target: s, ctx: context.Background(), timeout: time.Minute}
	sync := func() {
		t.Helper()
		if err := r.syncGit(sourceRepo, targetRepo); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	root, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	caches, err := filepath.Glob(filepath.Join(root, "gh-replicate", "git-v1", "*.git"))
	if err != nil || len(caches) != 1 {
		t.Fatalf("caches %v: %v", caches, err)
	}
	marker := filepath.Join(caches[0], "reused")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	base := gitTest(t, f.source, "rev-parse", "main")
	gitTest(t, f.destination, "update-ref", "refs/heads/exclusive", base)
	gitTest(t, f.destination, "update-ref", "refs/tags/exclusive", base)
	gitTest(t, f.source, "update-ref", "refs/heads/new", base)
	sync()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("cache was recreated", err)
	}
	gitTest(t, f.source, "update-ref", "-d", "refs/heads/new")
	gitTest(t, f.source, "update-ref", "-d", "refs/tags/v1")
	sync()
	if refs := gitRefs(t, caches[0]); strings.Contains(refs, "refs/heads/new") || strings.Contains(refs, "refs/tags/v1") {
		t.Fatal("stale source refs", refs)
	}
	for _, ref := range []string{"refs/heads/new", "refs/tags/v1", "refs/heads/exclusive", "refs/tags/exclusive"} {
		if got := gitTest(t, f.destination, "rev-parse", ref); got != base {
			t.Fatalf("lost destination ref %s", ref)
		}
	}
	// A stale push would recreate this ref, making the assertion observable.
	gitTest(t, f.destination, "update-ref", "-d", "refs/heads/main")
	before := gitRefs(t, f.destination)
	if err := os.Rename(f.source, f.source+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := r.syncGit(sourceRepo, targetRepo); err == nil || !strings.Contains(err.Error(), "fetch source") {
		t.Fatalf("fetch failure: %v", err)
	}
	if got := gitRefs(t, f.destination); got != before {
		t.Fatal("failed fetch changed destination")
	}
	if err := os.Rename(f.source+".offline", f.source); err != nil {
		t.Fatal(err)
	}
	sync() // A failed fetch must also release the lock.
}

func TestLFSHistoryAndObsoleteCache(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "commit", "--allow-empty", "-m", "clean")
	clean := gitTest(t, dir, "rev-parse", "HEAD")
	pointer := []byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 12345\n")
	if err := os.WriteFile(filepath.Join(dir, "pointer"), pointer, 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "pointer")
	gitTest(t, dir, "commit", "-m", "LFS")
	gitTest(t, dir, "rm", "pointer")
	gitTest(t, dir, "commit", "-m", "remove file but retain history")
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if err := r.checkLFS(dir); err == nil {
		t.Fatal("historical LFS escaped detection")
	}
	gitTest(t, dir, "reset", "--hard", clean)
	if err := r.checkLFS(dir); err != nil {
		t.Fatal("obsolete objects blocked clean source", err)
	}
}
