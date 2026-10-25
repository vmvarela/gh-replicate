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
	// A cached ref the source renamed into its own path must not block the
	// fetch: one atomic fetch cannot delete and create these refs together.
	gitTest(t, caches[0], "update-ref", "refs/heads/release", base)
	gitTest(t, f.source, "update-ref", "refs/heads/release/v2", base)
	sync()
	if got := gitTest(t, f.destination, "rev-parse", "refs/heads/release/v2"); got != base {
		t.Fatalf("renamed ref %s", got)
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

func TestCacheLockActiveChild(t *testing.T) {
	if root := os.Getenv("GH_REPLICATE_ACTIVE_CHILD"); root != "" {
		if os.Args[len(os.Args)-1] == "cache-child" {
			if err := os.WriteFile(filepath.Join(root, "started"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, "finish")); err == nil {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("child release timed out")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		lock, err := lockCache(filepath.Join(root, "lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		r := &replicator{cacheLock: lock, ctx: context.Background(), timeout: 20 * time.Second}
		// A real Git process must retain the lock while its command is active.
		alias := `!"` + filepath.ToSlash(os.Args[0]) + `" -test.run=^TestCacheLockActiveChild$ cache-child`
		if err := r.command(root, side{}, "git", "-c", "alias.cache-child="+alias, "cache-child"); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	owner := exec.Command(os.Args[0], "-test.run=^TestCacheLockActiveChild$")
	owner.Env = append(os.Environ(), "GH_REPLICATE_ACTIVE_CHILD="+root)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(filepath.Join(root, "finish"), nil, 0600)
		_ = owner.Process.Kill()
		_ = owner.Wait()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	if f, err := lockCache(filepath.Join(root, "lock")); err == nil {
		f.Close()
		t.Fatal("lock released while child is still active")
	}
	if err := os.WriteFile(filepath.Join(root, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		if f, err := lockCache(filepath.Join(root, "lock")); err == nil {
			f.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock not released after child exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSyncGitNewObjectsAndSecondTarget(t *testing.T) {
	f := newGitFixture(t)
	seamStub(t)
	second := filepath.Join(t.TempDir(), "second.git")
	gitTest(t, filepath.Dir(second), "init", "--bare", second)
	gitRemote = func(repo string) string {
		switch repo {
		case sourceRepo:
			return f.source
		case targetRepo:
			return f.destination
		default:
			return second
		}
	}
	s := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, `{"enabled":false}`) }), "token")
	r := &replicator{source: s, target: s, ctx: context.Background(), timeout: time.Minute}
	if err := r.syncGit(sourceRepo, targetRepo); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	gitTest(t, filepath.Dir(work), "clone", f.source, work)
	gitTest(t, work, "config", "user.name", "Test")
	gitTest(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("new objects"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, work, "add", "new.txt")
	gitTest(t, work, "commit", "-m", "new content")
	gitTest(t, work, "tag", "-f", "v1")
	gitTest(t, work, "push", "--force", "origin", "main", "refs/tags/v1")
	want := gitTest(t, work, "rev-parse", "HEAD")
	for _, target := range []string{targetRepo, "bob/second"} {
		if err := r.syncGit(sourceRepo, target); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{"main", "refs/tags/v1"} {
			if got := gitTest(t, gitRemote(target), "rev-parse", ref); got != want {
				t.Fatalf("%s %s: %s != %s", target, ref, got, want)
			}
		}
		if got := gitTest(t, gitRemote(target), "show", "main:new.txt"); got != "new objects" {
			t.Fatal(got)
		}
	}
	root, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	caches, err := filepath.Glob(filepath.Join(root, "gh-replicate", "git-v1", "*.git"))
	if err != nil || len(caches) != 1 {
		t.Fatalf("expected shared source cache: %v %v", caches, err)
	}
}

// A local SHA-256 default must not leak into the cache: GitHub serves SHA-1.
func TestSyncGitPinsSHA1ObjectFormat(t *testing.T) {
	f := newGitFixture(t)
	seamStub(t)
	t.Setenv("GIT_DEFAULT_HASH", "sha256")
	gitRemote = func(repo string) string {
		if repo == sourceRepo {
			return f.source
		}
		return f.destination
	}
	s := fakeSide(t, fakeServer(t, func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, `{"enabled":false}`) }), "token")
	r := &replicator{source: s, target: s, ctx: context.Background(), timeout: time.Minute}
	if err := r.syncGit(sourceRepo, targetRepo); err != nil {
		t.Fatal(err)
	}
	root, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	caches, err := filepath.Glob(filepath.Join(root, "gh-replicate", "git-v1", "*.git"))
	if err != nil || len(caches) != 1 {
		t.Fatalf("caches %v: %v", caches, err)
	}
	if got := gitTest(t, caches[0], "rev-parse", "--show-object-format"); got != "sha1" {
		t.Fatalf("cache object format %s", got)
	}
}

func TestLFSStreamingLargeListing(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%d", i)), []byte(fmt.Sprintf("content %d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "more than a pipe buffer of objects")
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if err := r.checkLFS(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	if err := r.checkLFS(dir); err == nil {
		t.Fatal("cancelled scan succeeded")
	}
}
