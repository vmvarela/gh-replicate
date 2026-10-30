package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func putLFSObject(t *testing.T, root string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	oid := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, "objects", oid[:2], oid[2:4])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, oid), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return oid
}

func TestLFSVerifyObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	oid := putLFSObject(t, root, []byte("hello"))
	oid2 := putLFSObject(t, root, []byte("world!"))

	if err := verifyLFSObjects(ctx, root, lfsInventory{oid: 5, oid2: 6}); err != nil {
		t.Fatalf("good objects: %v", err)
	}
	if err := verifyLFSObjects(ctx, root, lfsInventory{oid: 5, oid2: 7}); err == nil {
		t.Fatal("second object's wrong size accepted")
	}
	dirOID := sha256.Sum256([]byte("dir"))
	d := hex.EncodeToString(dirOID[:])
	if err := os.MkdirAll(filepath.Join(root, "objects", d[:2], d[2:4], d), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyLFSObjects(ctx, root, lfsInventory{d: 0}); err == nil {
		t.Fatal("non-regular path accepted")
	}
	done, stop := context.WithCancel(ctx)
	stop()
	if err := verifyLFSObjects(done, root, lfsInventory{}); err != context.Canceled {
		t.Fatalf("empty inventory, canceled ctx: got %v", err)
	}
	if err := verifyLFSObjects(ctx, root, lfsInventory{}); err != nil {
		t.Fatalf("empty inventory: %v", err)
	}
	if err := verifyLFSObjects(ctx, root, lfsInventory{oid: 6}); err == nil {
		t.Fatal("wrong size accepted")
	}
	missing := sha256.Sum256([]byte("x"))
	if err := verifyLFSObjects(ctx, root, lfsInventory{hex.EncodeToString(missing[:]): 1}); err == nil {
		t.Fatal("missing object accepted")
	}
	if err := verifyLFSObjects(ctx, root, lfsInventory{"../x": 1}); err == nil {
		t.Fatal("invalid oid accepted")
	}

	p := filepath.Join(root, "objects", oid[:2], oid[2:4], oid)
	if err := os.WriteFile(p, []byte("HELLO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyLFSObjects(ctx, root, lfsInventory{oid: 5}); err == nil {
		t.Fatal("corrupt same-size object accepted")
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := verifyLFSObjects(cctx, root, lfsInventory{oid: 5}); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
