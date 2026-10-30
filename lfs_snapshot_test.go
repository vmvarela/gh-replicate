package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func snapGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// snapSource builds a bare store with a branch, an annotated tag and annotated
// tags to a blob and a tree; returns the dir and its captured refs.
func snapSource(t *testing.T) (string, []gitRef) {
	t.Helper()
	work := t.TempDir()
	snapGit(t, work, "init", "-q", "-b", "main", "--object-format=sha1")
	must(t, os.WriteFile(filepath.Join(work, "f"), []byte("a\n"), 0o600))
	snapGit(t, work, "add", "f")
	snapGit(t, work, "commit", "-qm", "c1")
	snapGit(t, work, "tag", "-a", "v1", "-m", "v1")
	snapGit(t, work, "tag", "-a", "blobtag", "-m", "b", snapGit(t, work, "rev-parse", "HEAD:f"))
	snapGit(t, work, "tag", "-a", "treetag", "-m", "t", snapGit(t, work, "rev-parse", "HEAD^{tree}"))
	snapGit(t, work, "branch", "gone")
	src := filepath.Join(t.TempDir(), "src.git")
	snapGit(t, work, "clone", "-q", "--bare", "--no-local", work, src)
	return src, snapRefs(t, src)
}

func snapRefs(t *testing.T, dir string) []gitRef {
	t.Helper()
	var refs []gitRef
	for _, l := range strings.Split(snapGit(t, dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags"), "\n") {
		if id, n, ok := strings.Cut(l, " "); ok {
			refs = append(refs, gitRef{Name: n, ID: id})
		}
	}
	return refs
}

func snapFmt(refs []gitRef) string {
	var b strings.Builder
	for _, r := range refs {
		b.WriteString(r.ID + " " + r.Name + "\n")
	}
	return b.String()
}

func TestLFSSnapshotMaterializesExactRefs(t *testing.T) {
	src, refs := snapSource(t)
	a := newAux(t)
	must(t, a.materializeRefs(context.Background(), src, refs))
	if got := snapFmt(snapRefs(t, a.Dir)); got != snapFmt(refs) {
		t.Fatalf("refs\n%s\nwant\n%s", got, snapFmt(refs))
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("alternates left behind: %v", err)
	}
	if h := snapGit(t, a.Dir, "symbolic-ref", "HEAD"); h != "refs/heads/master" && h != "refs/heads/main" {
		t.Fatalf("HEAD touched: %s", h)
	}
	for tag, typ := range map[string]string{"v1": "commit", "blobtag": "blob", "treetag": "tree"} {
		if got := snapGit(t, a.Dir, "cat-file", "-t", "refs/tags/"+tag); got != "tag" {
			t.Fatalf("%s is %s, want annotated tag", tag, got)
		}
		if got := snapGit(t, a.Dir, "cat-file", "-t", "refs/tags/"+tag+"^{}"); got != typ {
			t.Fatalf("%s peels to %s want %s", tag, got, typ)
		}
	}

	// Mutate and GC the source: the snapshot is self-contained and unchanged.
	snapGit(t, src, "update-ref", "-d", "refs/heads/gone")
	snapGit(t, src, "update-ref", "-d", "refs/tags/v1")
	snapGit(t, src, "update-ref", "refs/heads/later", refs[0].ID)
	snapGit(t, src, "reflog", "expire", "--expire=now", "--all")
	snapGit(t, src, "gc", "-q", "--prune=now")
	snapGit(t, a.Dir, "fsck", "--connectivity-only", "--no-dangling")

	dst := filepath.Join(t.TempDir(), "dst.git")
	snapGit(t, t.TempDir(), "init", "-q", "--bare", dst)
	snapGit(t, a.Dir, "push", "-q", dst, refs[0].ID+":refs/heads/excl")
	snapGit(t, a.Dir, "push", "-q", "--atomic", "--force", dst, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	got := snapRefs(t, dst)
	var mirrored []gitRef
	for _, r := range got {
		if r.Name == "refs/heads/excl" {
			if r.ID != refs[0].ID {
				t.Fatal("exclusive ref changed")
			}
			continue
		}
		mirrored = append(mirrored, r)
	}
	if len(mirrored) == len(got) {
		t.Fatal("exclusive ref lost")
	}
	if snapFmt(mirrored) != snapFmt(refs) {
		t.Fatalf("destination\n%s\nwant\n%s", snapFmt(mirrored), snapFmt(refs))
	}
}

func TestLFSSnapshotAtomicReject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook")
	}
	src, refs := snapSource(t)
	var c1 string
	for _, r := range refs {
		if r.Name == "refs/heads/main" {
			c1 = r.ID
		}
	}
	work := t.TempDir()
	snapGit(t, work, "clone", "-q", src, "w")
	w := filepath.Join(work, "w")
	snapGit(t, w, "commit", "-q", "--allow-empty", "-m", "c2")
	c2 := snapGit(t, w, "rev-parse", "HEAD")
	snapGit(t, w, "push", "-q", "origin", "HEAD:refs/heads/main")
	a := newAux(t)
	must(t, a.materializeRefs(context.Background(), src, []gitRef{{"refs/heads/good", c2}, {"refs/heads/reject", c2}}))

	for _, atomic := range []bool{false, true} {
		dst := filepath.Join(t.TempDir(), "dst.git")
		snapGit(t, t.TempDir(), "init", "-q", "--bare", dst)
		snapGit(t, src, "push", "-q", dst, c1+":refs/heads/good")
		must(t, os.WriteFile(filepath.Join(dst, "hooks", "update"), []byte("#!/bin/sh\n[ \"$1\" = refs/heads/reject ] && exit 1\nexit 0\n"), 0o700))
		args := []string{"push", "-q", "--force", dst, "refs/heads/*:refs/heads/*"}
		if atomic {
			args = append(args[:1], append([]string{"--atomic"}, args[1:]...)...)
		}
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = a.Dir, append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if err := cmd.Run(); err == nil {
			t.Fatalf("atomic=%v: push must fail", atomic)
		}
		want := c2
		if atomic {
			want = c1
		}
		if got := snapGit(t, dst, "rev-parse", "refs/heads/good"); got != want {
			t.Fatalf("atomic=%v: good=%s want %s", atomic, got, want)
		}
		if snapGit(t, dst, "for-each-ref", "refs/heads/reject") != "" {
			t.Fatalf("atomic=%v: reject created", atomic)
		}
	}
}

func TestLFSSnapshotMissingObjectFailsClosed(t *testing.T) {
	src, refs := snapSource(t)
	// Unknown object: update-ref refuses.
	a := newAux(t)
	if err := a.materializeRefs(context.Background(), src, []gitRef{{"refs/heads/x", strings.Repeat("1", 40)}}); err == nil {
		t.Fatal("missing tip must fail")
	}
	// Tip present but a reachable blob is gone: repack/fsck refuse.
	loose := t.TempDir()
	snapGit(t, loose, "init", "-q", "--bare", "l.git")
	l := filepath.Join(loose, "l.git")
	snapGit(t, src, "push", "-q", l, "refs/heads/main")
	blob := snapGit(t, l, "rev-parse", "refs/heads/main:f")
	// small pushes stay loose (receive.unpackLimit=100), so the blob is a single file
	must(t, os.Remove(filepath.Join(l, "objects", blob[:2], blob[2:])))
	b := newAux(t)
	var main string
	for _, r := range refs {
		if r.Name == "refs/heads/main" {
			main = r.ID
		}
	}
	if err := b.materializeRefs(context.Background(), l, []gitRef{{"refs/heads/main", main}}); err == nil {
		t.Fatal("missing reachable object must fail")
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("alternates left after failure: %v", err)
	}
}

func TestLFSSnapshotRejectsBadInputAndEmpty(t *testing.T) {
	a := newAux(t)
	id := strings.Repeat("a", 40)
	bad := []gitRef{
		{"HEAD", id}, {"refs/remotes/x", id}, {"refs/heads/", id}, {"refs/heads/a..b", id},
		{"refs/heads/a b", id}, {"refs/heads/.x", id}, {"refs/heads/x.lock", id}, {"refs/heads/x/", id}, {"refs/heads/x.", id}, {"refs/heads//x", id}, {"refs/heads/a\x7fb", id}, {"refs/heads/a\tb", id}, {"refs/heads/a\x00b", id},
		{"refs/tags/a@{1}", id}, {"refs/heads/a\nb", id}, {"refs/heads/x", strings.Repeat("a", 64)},
		{"refs/heads/x", "HEAD"}, {"refs/heads/x", strings.ToUpper(id)},
	}
	for _, r := range bad {
		if err := a.materializeRefs(context.Background(), "/nonexistent", []gitRef{r}); err == nil {
			t.Fatalf("accepted %q", r)
		}
	}
	if err := a.materializeRefs(context.Background(), "/nonexistent", []gitRef{{"refs/heads/x", id}, {"refs/heads/x", id}}); err == nil {
		t.Fatal("accepted duplicate")
	}
	// Empty: no-op even with a bogus source.
	must(t, a.materializeRefs(context.Background(), "/nonexistent", nil))
	if snapGit(t, a.Dir, "for-each-ref") != "" {
		t.Fatal("empty snapshot created refs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src, refs := snapSource(t)
	if err := a.materializeRefs(ctx, src, refs); err == nil {
		t.Fatal("cancelled context must fail")
	}
	if snapGit(t, a.Dir, "for-each-ref") != "" {
		t.Fatal("cancelled snapshot created refs")
	}
}

func TestLFSSnapshotValidatorMatchesGit(t *testing.T) {
	names := []string{"@", "foo./bar", "a.b", "a/b/c", "é", "-x", "a@b", "x.lock", "a/.b", "a/b.lock/c", "a..b", "a b", "x.", "a//b", "a/", "a@{b", "a~", "a^", "a:", "a?", "a*", "a[", `a\b`, "a\x7f", "a\tb", "/a"}
	for _, pre := range []string{"refs/heads/", "refs/tags/"} {
		for _, n := range names {
			full := pre + n
			err := exec.Command("git", "check-ref-format", full).Run()
			if got := validSnapshotRefName(full); got != (err == nil) {
				t.Errorf("%q: validator=%v git valid=%v", full, got, err == nil)
			}
		}
	}
	// Valid-but-unusual names really round-trip through update-ref --stdin.
	src, refs := snapSource(t)
	id := refs[0].ID
	a := newAux(t)
	in := []gitRef{{"refs/heads/@", id}, {"refs/heads/foo./bar", id}, {"refs/tags/@", id}, {"refs/tags/foo./bar", id}}
	must(t, a.materializeRefs(context.Background(), src, in))
	got := map[string]string{}
	for _, r := range snapRefs(t, a.Dir) {
		got[r.Name] = r.ID
	}
	if len(got) != len(in) {
		t.Fatalf("refs %v", got)
	}
	for _, r := range in {
		if got[r.Name] != r.ID {
			t.Fatalf("missing %s", r.Name)
		}
	}
}

func TestLFSSnapshotFailsClosedOnExistingRefs(t *testing.T) {
	src, refs := snapSource(t)
	for _, packed := range []bool{false, true} {
		for _, ref := range []string{"refs/heads/old", "refs/notes/x", "refs/foo/bar"} {
			for name, mat := range map[string][]gitRef{"empty": nil, "refs": refs} {
				a := newAux(t)
				tree := snapGit(t, a.Dir, "mktree") // empty stdin: empty tree
				c := snapGit(t, a.Dir, "commit-tree", "-m", "old", tree)
				snapGit(t, a.Dir, "update-ref", ref, c)
				if packed {
					snapGit(t, a.Dir, "pack-refs", "--all")
				}
				before := snapGit(t, a.Dir, "for-each-ref")
				if err := a.materializeRefs(context.Background(), src, mat); err == nil {
					t.Fatalf("%s/%s packed=%v: must fail", name, ref, packed)
				}
				if after := snapGit(t, a.Dir, "for-each-ref"); after != before {
					t.Fatalf("%s/%s: refs changed\n%s\n%s", name, ref, before, after)
				}
				if _, err := os.Stat(filepath.Join(a.Dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
					t.Fatalf("alternates left: %v", err)
				}
			}
		}
	}
}

func TestLFSSnapshotRejectsDanglingSymbolicRefs(t *testing.T) {
	src, refs := snapSource(t)
	for _, ref := range []string{"refs/heads/unexpected", "refs/heads/main"} {
		t.Run(ref, func(t *testing.T) {
			a := newAux(t)
			target := "refs/heads/main"
			if ref == target {
				target = "refs/heads/missing"
			}
			snapGit(t, a.Dir, "symbolic-ref", ref, target)
			if got := snapGit(t, a.Dir, "for-each-ref"); got != "" {
				t.Fatalf("dangling symbolic ref unexpectedly visible: %s", got)
			}
			if err := a.materializeRefs(context.Background(), src, refs); err == nil {
				t.Fatal("snapshot accepted a dangling symbolic ref")
			}
			if _, err := os.Stat(filepath.Join(a.Dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
				t.Fatalf("alternates left after failure: %v", err)
			}
		})
	}
}
