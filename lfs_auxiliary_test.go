package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newAux(t *testing.T) *lfsAux {
	t.Helper()
	a, err := newLFSAux(context.Background())
	must(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestLFSAuxIsolation(t *testing.T) {
	tpl := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(tpl, "hooks"), 0o700))
	must(t, os.WriteFile(filepath.Join(tpl, "hooks", "pre-commit"), []byte("x"), 0o700))
	must(t, os.WriteFile(filepath.Join(tpl, "config"), []byte("[evil]\n\tx=1\n"), 0o600))
	gc := filepath.Join(t.TempDir(), "gc")
	must(t, os.WriteFile(gc, []byte("[core]\n\tfsmonitor=evil\n[evil]\n\tg=1\n"), 0o600))
	t.Setenv("GIT_TEMPLATE_DIR", tpl)
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "evil.env")
	t.Setenv("GIT_CONFIG_VALUE_0", "1")
	t.Setenv("GIT_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "secret-token")

	a := newAux(t)
	b := newAux(t)
	root := a.root
	if b.root == root {
		t.Fatal("second side must be distinct")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(root)
		must(t, err)
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
	for _, d := range []string{filepath.Join(root, "hooks"), filepath.Join(a.Dir, "hooks")} {
		ents, err := os.ReadDir(d)
		if err != nil && !os.IsNotExist(err) { // empty template: native hooks dir may not exist
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("%s not empty", d)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(a.Dir, "config"))
	must(t, err)
	if strings.Contains(string(cfg), "evil") || strings.Contains(string(cfg), "secret") {
		t.Fatalf("hostile config leaked: %s", cfg)
	}
	for _, e := range a.env {
		if strings.Contains(e, "secret-token") || strings.HasPrefix(e, "GIT_DIR=") || strings.HasPrefix(e, "GIT_TEMPLATE_DIR=") {
			t.Fatalf("leaked env %s", e)
		}
	}
	out, err := exec.Command("git", "--git-dir="+a.Dir, "rev-parse", "--is-bare-repository").Output()
	must(t, err)
	if strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("not bare: %s", out)
	}
	// sha1 objects have 40-hex ids.
	cmd := exec.Command("git", "--git-dir="+a.Dir, "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader("x")
	out, err = cmd.Output()
	must(t, err)
	if len(strings.TrimSpace(string(out))) != 40 {
		t.Fatalf("not sha1: %s", out)
	}
	for _, x := range []*lfsAux{a, b} {
		must(t, x.Close())
		must(t, x.Close())
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("not cleaned: %v", err)
	}
}

func TestLFSAuxEnvNoDuplicateWindowsNames(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range lfsAuxEnv(t.TempDir()) {
		k := strings.ToUpper(strings.SplitN(e, "=", 2)[0])
		if seen[k] {
			t.Fatalf("duplicate env %s", k)
		}
		seen[k] = true
	}
}

func TestLFSAuxRelativePathGitIsAbsoluteOrRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relative PATH semantics differ")
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o700))
	t.Chdir(dir)
	t.Setenv("PATH", ".")
	a, err := newLFSAux(context.Background())
	if err == nil {
		t.Cleanup(func() { _ = a.Close() })
		t.Fatalf("relative PATH git must not run (got %s)", a.git)
	}
}

func TestLFSAuxCleansOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if _, err := newLFSAux(ctx); err == nil {
		t.Fatal("expected error")
	}
	ents, err := os.ReadDir(tmp)
	must(t, err)
	if len(ents) != 0 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestLFSAuxWritePointers(t *testing.T) {
	a := newAux(t)
	sel, err := a.writePointers(context.Background(), lfsInventory{})
	must(t, err)
	if len(sel) != 0 {
		t.Fatal("empty inventory must yield no selectors")
	}
	for _, bad := range []lfsInventory{
		{"xyz": 1},
		{strings.Repeat("A", 64): 1},
		{strings.Repeat("a", 64): 0},
		{strings.Repeat("a", 64): -1},
	} {
		if _, err := a.writePointers(context.Background(), bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	inv := lfsInventory{}
	for i := 0; i < 3000; i++ {
		h := sha256.Sum256([]byte(strconv.Itoa(i)))
		inv[hex.EncodeToString(h[:])] = int64(i + 1)
	}
	sel, err = a.writePointers(context.Background(), inv)
	must(t, err)
	if len(sel) != len(inv) {
		t.Fatalf("%d selectors for %d objects", len(sel), len(inv))
	}
	// Stream every selector through cat-file and compare bytes and blob id.
	var q strings.Builder
	oids := make([]string, 0, len(sel))
	for oid, id := range sel {
		oids = append(oids, oid)
		q.WriteString(id + "\n")
	}
	cmd := exec.Command(a.git, "--git-dir="+a.Dir, "cat-file", "--batch")
	cmd.Env, cmd.Stdin = a.env, strings.NewReader(q.String())
	out, err := cmd.Output()
	must(t, err)
	r := bufio.NewReader(bytes.NewReader(out))
	seen := map[string]bool{}
	for _, oid := range oids {
		hdr, err := r.ReadString('\n')
		must(t, err)
		var id, typ string
		var n int
		if _, err := fmt.Sscanf(hdr, "%s %s %d", &id, &typ, &n); err != nil || typ != "blob" || id != sel[oid] || seen[id] {
			t.Fatalf("bad header %q for %s", hdr, oid)
		}
		seen[id] = true
		body := make([]byte, n+1)
		_, err = io.ReadFull(r, body)
		must(t, err)
		if string(body[:n]) != lfsCanonicalPointer(oid, inv[oid]) {
			t.Fatalf("pointer mismatch for %s: %q", oid, body[:n])
		}
		if lfs, err := exec.LookPath("git-lfs"); err == nil && len(seen) <= 3 {
			c := exec.Command(lfs, "pointer", "--check", "--strict", "--stdin")
			c.Env, c.Stdin = a.env, bytes.NewReader(body[:n])
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("git-lfs rejects pointer: %v %s", err, out)
			}
		}
	}
	refs := exec.Command(a.git, "--git-dir="+a.Dir, "for-each-ref")
	refs.Env = a.env
	out, err = refs.Output()
	must(t, err)
	if len(out) != 0 {
		t.Fatalf("refs created: %s", out)
	}
	if _, err := os.Stat(filepath.Join(a.root, "marks")); !os.IsNotExist(err) {
		t.Fatalf("marks left: %v", err)
	}
}

func TestLFSAuxWritePointersCanceled(t *testing.T) {
	a := newAux(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.writePointers(ctx, lfsInventory{}); err == nil {
		t.Fatal("canceled ctx with empty inventory must fail")
	}
	inv := lfsInventory{strings.Repeat("a", 64): 1}
	if _, err := a.writePointers(ctx, inv); err == nil {
		t.Fatal("canceled ctx must fail")
	}
}

func TestLFSParseMarks(t *testing.T) {
	id1, id2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	oids := []string{"a", "b"}
	if sel, err := parseLFSMarks(":1 "+id1+"\n:2 "+id2+"\n", oids); err != nil || sel["b"] != id2 {
		t.Fatalf("valid rejected: %v", err)
	}
	for _, bad := range []string{
		":1 " + id1,                                     // missing mark
		":1 " + id1 + "\n:1 " + id2,                     // duplicate mark
		":1 " + id1 + "\n:2 " + id1,                     // duplicate blob id
		":1 " + id1 + "\n:3 " + id2,                     // out of range
		":1 " + id1 + "\n:2 " + strings.Repeat("G", 40), // non-hex
		":1 " + id1 + "\n:2 " + strings.Repeat("A", 40), // uppercase
		":1 " + id1 + "\n:2 " + strings.Repeat("2", 64), // sha256-length id
		":1 " + id1 + "\n:2 " + id2 + " extra",
		":1 " + id1 + "\n:+2 " + id2,
		":1 " + id1 + "\n garbage",
	} {
		if _, err := parseLFSMarks(bad, oids); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestLFSSideEnvIsolation(t *testing.T) {
	t.Setenv("GIT_DIR", "/evil")
	t.Setenv("GH_TOKEN", "inherited-token")
	t.Setenv("GITHUB_TOKEN", "also-inherited")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	root := t.TempDir()
	env := lfsSideEnv(root, "side-token", "/usr/bin:/bin")

	got := map[string]string{}
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 {
			got[e[:i]] = e[i+1:]
		}
	}
	if got["GIT_DIR"] != "" {
		t.Fatalf("GIT_DIR leaked: %q", got["GIT_DIR"])
	}
	if got["GH_TOKEN"] != "side-token" {
		t.Fatalf("GH_TOKEN = %q, want side-token", got["GH_TOKEN"])
	}
	if got["GITHUB_TOKEN"] != "" {
		t.Fatalf("GITHUB_TOKEN leaked: %q", got["GITHUB_TOKEN"])
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q", got["PATH"])
	}
	if got["HTTPS_PROXY"] != "http://proxy:3128" {
		t.Fatalf("HTTPS_PROXY = %q", got["HTTPS_PROXY"])
	}
	if got["GIT_CONFIG_NOSYSTEM"] != "1" || got["GIT_CONFIG_GLOBAL"] != os.DevNull {
		t.Fatalf("git config isolation missing: %q", got)
	}
	if got["HOME"] != root || got["GH_CONFIG_DIR"] == "" {
		t.Fatalf("private home/config missing: %q", got)
	}
}

func TestLFSEndpoint(t *testing.T) {
	if got := lfsEndpoint("owner/repo"); got != "https://github.com/owner/repo.git/info/lfs" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestLFSAuxPointHEADAbsent(t *testing.T) {
	a := newAux(t)
	refs := []gitRef{{Name: "refs/heads/gh-replicate-no-lfsconfig", ID: strings.Repeat("1", 40)}}
	must(t, a.pointHEADAbsent(context.Background(), refs))
	// The collision guard must pick a name not present in the snapshot.
	if got := gitTest(t, a.Dir, "--git-dir="+a.Dir, "symbolic-ref", "HEAD"); got != "refs/heads/gh-replicate-no-lfsconfig_" {
		t.Fatalf("HEAD = %q", got)
	}
	if gitTest(t, a.Dir, "--git-dir="+a.Dir, "for-each-ref", "--format=%(refname)") != "" {
		t.Fatalf("absent ref was materialized")
	}
}
