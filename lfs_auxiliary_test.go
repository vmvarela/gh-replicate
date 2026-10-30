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
	must(t, a.pointHEADAbsent(context.Background()))
	// HEAD points outside the refs the fetch writes, and does not resolve.
	if got := gitTest(t, a.Dir, "--git-dir="+a.Dir, "symbolic-ref", "HEAD"); got != "refs/gh-replicate/no-lfsconfig" {
		t.Fatalf("HEAD = %q", got)
	}
	cmd := exec.Command("git", "--git-dir="+a.Dir, "rev-parse", "--verify", "HEAD")
	if err := cmd.Run(); err == nil {
		t.Fatal("absent HEAD unexpectedly resolves")
	}
}
