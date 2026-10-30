package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var lfsBareOIDRE = regexp.MustCompile(`\A[0-9a-f]{64}\z`)

// verifyLFSObjects checks that every inventory object exists under
// root/objects/aa/bb/OID as a regular file with the expected size and SHA-256.
// It fails closed on the first problem. root must already be a controlled
// directory (parent symlinks are not checked): it does not detect concurrent mutation nor that git-lfs later
// reads this storage.
func verifyLFSObjects(ctx context.Context, root string, inv lfsInventory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for oid, size := range inv {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !lfsBareOIDRE.MatchString(oid) {
			return fmt.Errorf("invalid Git LFS object id %q", oid)
		}
		if err := verifyLFSObject(ctx, filepath.Join(root, "objects", oid[:2], oid[2:4], oid), oid, size); err != nil {
			return err
		}
	}
	return nil
}

func verifyLFSObject(ctx context.Context, path, oid string, size int64) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("Git LFS object %s: %w", oid, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("Git LFS object %s is not a regular file", oid)
	}
	if fi.Size() != size {
		return fmt.Errorf("Git LFS object %s has size %d, expected %d", oid, fi.Size(), size)
	}
	f, err := os.Open(path) // #nosec G304 -- path is built from a validated lowercase 64-hex OID under the controlled LFS root
	if err != nil {
		return fmt.Errorf("Git LFS object %s: %w", oid, err)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, rerr := f.Read(buf)
		h.Write(buf[:m])
		n += int64(m)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("Git LFS object %s: %w", oid, rerr)
		}
	}
	if n != size {
		return fmt.Errorf("Git LFS object %s read %d bytes, expected %d", oid, n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		return fmt.Errorf("Git LFS object %s has SHA-256 %s", oid, got)
	}
	return nil
}
