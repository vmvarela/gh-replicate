package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// lfsAux is an ephemeral bare repository owned by one side of an LFS run.
// It never copies config, HEAD, refs, templates or hooks from the cache.
type lfsAux struct {
	Dir  string // bare repository
	root string
	env  []string
	git  string
}

// lfsAuxEnv is the scratch environment for a.run, which only invokes local git
// commands (init, config, symbolic-ref) and so needs no PATH. The transfer
// commands run through lfsSideEnv instead.
func lfsAuxEnv(root string) []string {
	env := []string{
		"HOME=" + root, "USERPROFILE=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "xdg"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_ATTR_NOSYSTEM=1", "LC_ALL=C",
	}
	if runtime.GOOS == "windows" {
		// Windows env names are case-insensitive: one spelling only.
		for _, k := range []string{"SystemRoot", "ComSpec"} {
			if v := os.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	return env
}

func (a *lfsAux) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, a.git, args...) // #nosec G204 -- resolved git binary, argv only
	cmd.Dir = a.root
	cmd.Env = a.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return nil
}

// newLFSAux creates a private (0700) bare repo with empty native hooks.
func newLFSAux(ctx context.Context) (*lfsAux, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	if git, err = filepath.Abs(git); err != nil {
		return nil, err
	}
	// 0700 on Windows does not prove a private ACL; that is not verified here.
	root, err := os.MkdirTemp("", "gh-replicate-lfs-")
	if err != nil {
		return nil, err
	}
	a := &lfsAux{Dir: filepath.Join(root, "repo.git"), root: root, env: lfsAuxEnv(root), git: git}
	hooks := filepath.Join(root, "hooks")
	empty := filepath.Join(root, "template")
	for _, d := range []string{hooks, empty} {
		if err = os.Mkdir(d, 0o700); err != nil {
			break
		}
	}
	if err == nil {
		err = a.run(ctx, "init", "--bare", "--quiet", "--object-format=sha1", "--template="+empty, a.Dir)
	}
	if err == nil {
		err = a.run(ctx, "--git-dir="+a.Dir, "config", "core.hooksPath", hooks)
	}
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}

// Close removes everything; safe to call repeatedly.
func (a *lfsAux) Close() error {
	if a == nil || a.root == "" {
		return nil
	}
	err := os.RemoveAll(a.root)
	if err == nil {
		a.root = ""
	}
	return err
}

// pointHEADAbsent sets HEAD to a ref guaranteed absent from the snapshot, so
// git-lfs cannot read a .lfsconfig committed in the replicated history. The
// namespace refs/gh-replicate/ is outside refs/heads and refs/tags (the only
// namespaces the fetch writes), so it cannot collide with a replicated branch
// on a case-insensitive filesystem.
func (a *lfsAux) pointHEADAbsent(ctx context.Context) error {
	const absent = "refs/gh-replicate/no-lfsconfig"
	if err := a.run(ctx, "--git-dir="+a.Dir, "symbolic-ref", "HEAD", absent); err != nil {
		return err
	}
	// Fail closed if HEAD unexpectedly resolves to a commit.
	cmd := exec.CommandContext(ctx, a.git, "--git-dir="+a.Dir, "rev-parse", "--verify", "HEAD") // #nosec G204 -- resolved git binary, argv only
	cmd.Dir, cmd.Env = a.root, a.env
	if err := cmd.Run(); err == nil {
		return errors.New("ephemeral HEAD resolves; refusing to run LFS")
	}
	return nil
}

// lfsEndpoint is the GitHub LFS API URL for a repo's HTTPS remote.
func lfsEndpoint(ownerRepo string) string {
	return gitRemote(ownerRepo) + "/info/lfs"
}

// lfsSideEnv is the controlled environment for one side's git/git-lfs run in
// the aux bare: built from scratch with a private home/config, only the side's
// token, no inherited GIT_*, and TLS left enabled. PATH and the transport
// profile (proxy/CA) that the existing git fetch already honors are preserved
// deliberately, so users behind a proxy or custom CA keep working.
func lfsSideEnv(root, token, path string) []string {
	env := []string{
		"HOME=" + root, "USERPROFILE=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "xdg"),
		"GH_CONFIG_DIR=" + filepath.Join(root, "gh"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_ATTR_NOSYSTEM=1", "LC_ALL=C",
	}
	if path != "" {
		env = append(env, "PATH="+path)
	}
	if token != "" {
		env = append(env, "GH_TOKEN="+token)
	}
	// Preserve both spellings: proxies conventionally honor lowercase, and the
	// previous gitEnv inherited everything, so dropping one would regress.
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy", "GIT_SSL_CAINFO", "SSL_CERT_FILE"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if runtime.GOOS == "windows" {
		for _, k := range []string{"SystemRoot", "ComSpec"} {
			if v := os.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	return env
}

// lfsCmd builds a git command in the aux bare for one side, with the side's
// isolated environment, its LFS endpoint pinned and the cache lock inherited.
func (r *replicator) lfsCmd(ctx context.Context, a *lfsAux, s side, ownerRepo string, args ...string) *exec.Cmd {
	full := append([]string{"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!gh auth git-credential",
		"-c", "lfs.url=" + lfsEndpoint(ownerRepo), "-c", "lfs.pushurl=" + lfsEndpoint(ownerRepo)}, args...)
	cmd := newLFSCmd(ctx, a.git, full...) // #nosec G204 -- resolved git binary, argv only
	cmd.Dir = a.Dir
	cmd.Env = lfsSideEnv(a.root, s.token, os.Getenv("PATH"))
	if r.cacheLock != nil {
		inheritCacheLock(cmd, r.cacheLock)
	}
	return cmd
}

func (r *replicator) lfsRun(ctx context.Context, a *lfsAux, s side, ownerRepo string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := r.lfsCmd(ctx, a, s, ownerRepo, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("git %s timed out after %s", args[0], r.timeout)
		}
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

// lfsRunIn is lfsRun with refs fed over stdin, for --stdin commands.
func (r *replicator) lfsRunIn(ctx context.Context, a *lfsAux, s side, ownerRepo string, stdin string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := r.lfsCmd(ctx, a, s, ownerRepo, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("git %s timed out after %s", args[0], r.timeout)
		}
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
