package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// lfsMinVersion is the oldest git-lfs version this code has been tested
// against (3.8.1); older versions are rejected rather than assumed compatible.
var lfsMinVersion = [3]int{3, 8, 1}

// parseLFSVersion reads the leading "git-lfs/X.Y.Z" token of `git lfs version`.
func parseLFSVersion(out string) ([3]int, error) {
	var v [3]int
	fields := strings.Fields(out)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "git-lfs/") {
		return v, fmt.Errorf("unexpected git-lfs version output %q", out)
	}
	parts := strings.Split(strings.TrimPrefix(fields[0], "git-lfs/"), ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("unexpected git-lfs version %q", fields[0])
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, fmt.Errorf("unexpected git-lfs version %q", fields[0])
		}
		v[i] = n
	}
	return v, nil
}

// requireLFS enforces the conditional dependency: only reached when LFS is
// confirmed, it fails if git-lfs is missing or older than the tested version.
func (r *replicator) requireLFS(ctx context.Context, a *lfsAux, s side, ownerRepo string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := r.lfsCmd(ctx, a, s, ownerRepo, "lfs", "version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git-lfs is required to replicate a repository with LFS objects: %w", err)
	}
	v, err := parseLFSVersion(string(out))
	if err != nil {
		return err
	}
	if v[0] < lfsMinVersion[0] || (v[0] == lfsMinVersion[0] && (v[1] < lfsMinVersion[1] || (v[1] == lfsMinVersion[1] && v[2] < lfsMinVersion[2]))) {
		return fmt.Errorf("git-lfs %d.%d.%d is too old; %d.%d.%d or newer is required", v[0], v[1], v[2], lfsMinVersion[0], lfsMinVersion[1], lfsMinVersion[2])
	}
	return nil
}

// transferLFS downloads every inventory object from the source, verifies its
// size and SHA-256, then uploads to the destination, all before any ref push.
// Pointers the CLI scanner finds (<1024 bytes) transfer by ref; oversized
// (>=1024 bytes) pointers transfer by materialized canonical pointer and
// --object-id, because the scanner rejects blobs at or above that size.
func (r *replicator) transferLFS(ctx context.Context, a *lfsAux, source, target string, refs []gitRef, inv lfsInventory, big map[string]bool) error {
	var names strings.Builder
	for _, ref := range refs {
		names.WriteString(ref.Name + "\n")
	}
	refsIn := names.String()
	if err := r.lfsRunIn(ctx, a, r.source, source, refsIn, "lfs", "fetch", "--all", "--stdin"); err != nil {
		return fmt.Errorf("download Git LFS objects from source: %w", err)
	}
	if len(big) > 0 {
		blobs, err := a.writePointers(ctx, inv, big)
		if err != nil {
			return fmt.Errorf("materialize oversized Git LFS pointers: %w", err)
		}
		if err := r.lfsRunIn(ctx, a, r.source, source, strings.Join(blobs, "\n")+"\n", "lfs", "fetch", "--all", "--stdin"); err != nil {
			return fmt.Errorf("download oversized Git LFS objects from source: %w", err)
		}
	}
	if err := verifyLFSObjects(ctx, filepath.Join(a.Dir, "lfs"), inv); err != nil {
		return fmt.Errorf("verify downloaded Git LFS objects: %w", err)
	}
	if err := r.lfsRunIn(ctx, a, r.target, target, refsIn, "lfs", "push", "--all", "--stdin", "replicate-target"); err != nil {
		return fmt.Errorf("upload Git LFS objects to target: %w", err)
	}
	if len(big) > 0 {
		var oids []string
		for oid := range big {
			oids = append(oids, oid)
		}
		sort.Strings(oids)
		if err := r.lfsRunIn(ctx, a, r.target, target, strings.Join(oids, "\n")+"\n", "lfs", "push", "--object-id", "--stdin", "replicate-target"); err != nil {
			return fmt.Errorf("upload oversized Git LFS objects to target: %w", err)
		}
	}
	return nil
}
