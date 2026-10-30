package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// materializeRefs makes a.Dir hold exactly refs (heads and tags, by object ID)
// with every reachable object copied into its own pack, read from the local
// bare store srcGitDir. Nothing runs inside srcGitDir: its config, hooks and
// HEAD are never read, and nothing is written there. a's HEAD is left alone.
//
// Objects are borrowed through a temporary alternates entry, packed locally
// with repack -a (which includes alternate objects), then the entry is removed
// and connectivity is checked with fsck. A missing or concurrently pruned
// object makes update-ref, repack or fsck fail: the step fails closed but this
// does not protect against a non-cooperating process deleting objects; run it
// under the cache lock. On error a is in an unspecified state: Close it.
// An empty refs still checks that the auxiliary has no visible refs.
func (a *lfsAux) materializeRefs(ctx context.Context, srcGitDir string, refs []gitRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var in strings.Builder
	seen := make(map[string]string, len(refs))
	for _, r := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validSnapshotRefName(r.Name) {
			return fmt.Errorf("invalid snapshot ref name %q", r.Name)
		}
		if !lfsBlobIDRE.MatchString(r.ID) { // aux is sha1-only
			return fmt.Errorf("invalid object id %q for %s", r.ID, r.Name)
		}
		if _, exists := seen[r.Name]; exists {
			return fmt.Errorf("duplicate snapshot ref %s", r.Name)
		}
		seen[r.Name] = r.ID
		fmt.Fprintf(&in, "option no-deref\ncreate %s %s\n", r.Name, r.ID)
	}
	// Fail closed on any pre-existing ref (any namespace, loose or packed):
	// wildcard pushes would otherwise publish refs outside the snapshot.
	cmd := exec.CommandContext(ctx, a.git, "--git-dir="+a.Dir, "for-each-ref", "--count=1", "--format=%(refname)") // #nosec G204 -- resolved git binary, argv only
	cmd.Dir, cmd.Env = a.root, a.env
	if out, err := cmd.Output(); err != nil {
		return fmt.Errorf("git for-each-ref: %w", err)
	} else if len(strings.TrimSpace(string(out))) != 0 {
		return fmt.Errorf("auxiliary repository already has refs (%s)", strings.TrimSpace(string(out)))
	}
	if len(refs) == 0 {
		return nil
	}
	src, err := filepath.Abs(filepath.Join(srcGitDir, "objects"))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return fmt.Errorf("snapshot source objects %s: not a directory", src)
	}
	if strings.ContainsAny(src, "\n\r") {
		return errors.New("snapshot source path contains a newline")
	}
	alt := filepath.Join(a.Dir, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(src+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(alt)
	// One transaction over stdin: no argv limit; "create" fails if a ref exists.
	cmd = exec.CommandContext(ctx, a.git, "--git-dir="+a.Dir, "update-ref", "--stdin") // #nosec G204 -- resolved git binary, argv only
	cmd.Dir, cmd.Env, cmd.Stdin = a.root, a.env, strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, out)
	}
	// Broken symbolic refs are invisible until their target exists.
	cmd = exec.CommandContext(ctx, a.git, "--git-dir="+a.Dir, "for-each-ref", "--format=%(refname) %(objectname)") // #nosec G204 -- resolved git binary, argv only
	cmd.Dir, cmd.Env = a.root, a.env
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("verify snapshot refs: %w", err)
	}
	remaining := len(seen)
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		name, id, ok := strings.Cut(line, " ")
		if !ok || seen[name] != id || id == "" {
			return fmt.Errorf("unexpected snapshot ref %q", line)
		}
		delete(seen, name)
		remaining--
	}
	if remaining != 0 {
		return errors.New("snapshot refs are incomplete")
	}
	if err := a.run(ctx, "--git-dir="+a.Dir, "repack", "-a", "-d", "-q"); err != nil {
		return err
	}
	if err := os.Remove(alt); err != nil {
		return err
	}
	return a.run(ctx, "--git-dir="+a.Dir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
}

// validSnapshotRefName is a conservative subset of git check-ref-format,
// limited to refs/heads/ and refs/tags/. A "@" or a component ending in "."
// is valid in Git; only the whole name may not end in ".". Spaces, control
// characters and newlines stay rejected: they would break update-ref --stdin.
func validSnapshotRefName(n string) bool {
	rest, ok := strings.CutPrefix(n, "refs/heads/")
	if !ok {
		if rest, ok = strings.CutPrefix(n, "refs/tags/"); !ok {
			return false
		}
	}
	if rest == "" || strings.Contains(n, "..") || strings.Contains(n, "@{") {
		return false
	}
	for _, c := range n {
		if c <= ' ' || c == 0x7f || strings.ContainsRune(`~^:?*[\`, c) {
			return false
		}
	}
	if strings.HasSuffix(n, ".") {
		return false
	}
	for _, p := range strings.Split(rest, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}
