package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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

// Env is built from scratch: no inherited GIT_*, PATH, HOME or tokens.
// shortcut: no PATH, so git helpers/git-lfs are not found; add when transfers are wired.
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

// lfsCanonicalPointer is the exact pointer text git-lfs writes (spec v1, no extensions).
func lfsCanonicalPointer(oid string, size int64) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
}

// writePointers stores one canonical pointer blob per inventory object and
// returns oid -> blob id, an unambiguous selector. fast-import streams the
// blobs over stdin (no argv limits) and, with no commit/reset commands,
// creates no refs. An empty inventory runs nothing.
func (a *lfsAux) writePointers(ctx context.Context, inv lfsInventory) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	oids := make([]string, 0, len(inv))
	for oid, size := range inv {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !lfsBareOIDRE.MatchString(oid) {
			return nil, fmt.Errorf("invalid Git LFS object id %q", oid)
		}
		if size <= 0 {
			return nil, fmt.Errorf("invalid Git LFS size %d for object %s", size, oid)
		}
		oids = append(oids, oid)
	}
	sel := make(map[string]string, len(oids))
	if len(oids) == 0 {
		return sel, nil
	}
	sort.Strings(oids)
	var in bytes.Buffer
	// shortcut: fast-import gets the whole input as one in-memory buffer (~150 B
	// per object); switch to an io.Pipe writer if inventories reach millions of objects.
	for i, oid := range oids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := lfsCanonicalPointer(oid, inv[oid])
		fmt.Fprintf(&in, "blob\nmark :%d\ndata %d\n%s\n", i+1, len(p), p)
	}
	in.WriteString("done\n")
	marks := filepath.Join(a.root, "marks")
	defer os.Remove(marks)
	cmd := exec.CommandContext(ctx, a.git, "--git-dir="+a.Dir, "fast-import", "--quiet", "--done", "--export-marks="+marks) // #nosec G204 -- resolved git binary, argv only
	cmd.Dir, cmd.Env, cmd.Stdin = a.root, a.env, &in
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git fast-import: %w: %s", err, out)
	}
	data, err := os.ReadFile(marks)
	if err != nil {
		return nil, err
	}
	return parseLFSMarks(string(data), oids)
}

// parseLFSMarks fails closed: exactly one ":N <40 lowercase hex>" line per
// mark 1..len(oids), no duplicates, no extra fields.
func parseLFSMarks(data string, oids []string) (map[string]string, error) {
	sel := make(map[string]string, len(oids))
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(f[0], ":") || !lfsBlobIDRE.MatchString(f[1]) {
			return nil, fmt.Errorf("unexpected fast-import mark %q", line)
		}
		mark, err := strconv.Atoi(f[0][1:])
		if err != nil || mark < 1 || mark > len(oids) || f[0] != ":"+strconv.Itoa(mark) {
			return nil, fmt.Errorf("unexpected fast-import mark %q", line)
		}
		if _, dup := sel[oids[mark-1]]; dup || seen[f[1]] {
			return nil, fmt.Errorf("duplicate fast-import mark %q", line)
		}
		seen[f[1]] = true
		sel[oids[mark-1]] = f[1]
	}
	if len(sel) != len(oids) {
		return nil, fmt.Errorf("fast-import returned %d of %d pointers", len(sel), len(oids))
	}
	return sel, nil
}

var lfsBlobIDRE = regexp.MustCompile(`\A[0-9a-f]{40}\z`)

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

// lfsStandaloneAgent is empty in production: GitHub uses basic HTTPS transfer.
// Tests override it to "lfs-standalone-file" to exercise file:// remotes locally.
var lfsStandaloneAgent = ""

// lfsCmd builds a git command in the aux bare for one side, with the side's
// isolated environment, its LFS endpoint pinned and the cache lock inherited.
func (r *replicator) lfsCmd(ctx context.Context, a *lfsAux, s side, ownerRepo string, args ...string) *exec.Cmd {
	full := append([]string{"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!gh auth git-credential",
		"-c", "lfs.url=" + lfsEndpoint(ownerRepo), "-c", "lfs.pushurl=" + lfsEndpoint(ownerRepo),
		"-c", "lfs.standalonetransferagent=" + lfsStandaloneAgent}, args...)
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
