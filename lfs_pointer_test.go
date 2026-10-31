package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	ptrOID  = "sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	ptrHex  = "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	ptrV1   = "https://git-lfs.github.com/spec/v1"
	hawser  = "https://hawser.github.com/spec/v1"
	media   = "http://git-media.io/v/2"
	ptrBody = "oid " + ptrOID + "\nsize 12345\n"
)

func pad(s string, n int) string { return s + strings.Repeat("\n", n-len(s)) }

type lfsCase struct {
	name    string
	data    string
	blocked bool // expected blocked (alias present but not confirmed)
	found   bool
}

func lfsCases() []lfsCase {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	long := "version " + ptrV1 + "\n" + ptrBody
	return []lfsCase{
		{name: "lfs", data: long, found: true},
		{name: "hawser", data: "version " + hawser + "\n" + ptrBody, found: true},
		{name: "git-media", data: "version " + media + "\n" + ptrBody, found: true},
		{name: "crlf", data: crlf(long), found: true},
		{name: "leading whitespace", data: " \n\t" + long, found: true},
		{name: "no trailing newline", data: strings.TrimSuffix(long, "\n"), found: true},
		{name: "padded 1023", data: pad(long, 1023), found: true},
		{name: "padded 1024", data: pad(long, 1024), found: true},
		{name: "padded 1025", data: pad(long, 1025), found: true},
		{name: "padded 2127", data: pad(long, 2127), found: true},
		{name: "blank lines between", data: "version " + ptrV1 + "\n\n" + ptrBody, found: true},
		{name: "extension", data: "version " + ptrV1 + "\next-0-foo " + ptrOID + "\n" + ptrBody, found: true},
		{name: "extension before version", data: "ext-0-foo " + ptrOID + "\n" + long, found: true},
		{name: "swapped order", data: "version " + ptrV1 + "\nsize 12345\noid " + ptrOID + "\n", blocked: true},
		{name: "missing size", data: "version " + ptrV1 + "\noid " + ptrOID + "\n", blocked: true},
		{name: "plus size", data: "version " + ptrV1 + "\noid " + ptrOID + "\nsize +5\n", blocked: true},
		{name: "leading zero size", data: "version " + ptrV1 + "\noid " + ptrOID + "\nsize 05\n", blocked: true},
		{name: "overflow size", data: "version " + ptrV1 + "\noid " + ptrOID + "\nsize 9223372036854775808\n", blocked: true},
		{name: "uppercase oid", data: "version " + ptrV1 + "\noid sha256:" + strings.ToUpper(ptrHex) + "\nsize 5\n", blocked: true},
		{name: "extra line", data: long + "extra x\n", blocked: true},
		{name: "trailing space in version", data: "version " + ptrV1 + " \n" + ptrBody, blocked: true},
		{name: "indented version line", data: "oid " + ptrOID + "\n version " + ptrV1 + "\nsize 5\n", blocked: true},
		{name: "prose", data: "see version " + ptrV1 + " for details\n"},
		{name: "plain text", data: "hello\n"},
		{name: "unknown future version", data: "version https://git-lfs.github.com/spec/v2\n" + ptrBody, blocked: true},
	}
}

func classifyCase(data string) (lfsPointer, bool, string) {
	b := []byte(data)
	n := min(len(b), lfsPrefixSize)
	return classifyLFSBlob(b[:n], int64(len(b)))
}

func TestLFSClassifyBlob(t *testing.T) {
	for _, c := range lfsCases() {
		t.Run(c.name, func(t *testing.T) {
			p, found, blocked := classifyCase(c.data)
			if found != c.found || (blocked != "") != c.blocked {
				t.Fatalf("found=%v blocked=%q, want found=%v blocked=%v", found, blocked, c.found, c.blocked)
			}
			if found && (p.OID != ptrHex || p.Size != 12345) {
				t.Fatalf("pointer = %+v", p)
			}
		})
	}
}

// A size-0 pointer names an empty object; git-lfs stores nothing for it, so it
// is classified as "none" (nothing to transfer) rather than blocked.
func TestLFSClassifyZeroSizeNone(t *testing.T) {
	data := "version " + ptrV1 + "\noid " + ptrOID + "\nsize 0\n"
	if _, found, blocked := classifyCase(data); found || blocked != "" {
		t.Fatalf("found=%v blocked=%q, want none", found, blocked)
	}
}

// A cut that lands inside content could change a value, so it must block.
func TestLFSClassifyBlobCutInsideContent(t *testing.T) {
	head := "version " + ptrV1 + "\noid " + ptrOID + "\nsize "
	data := head + strings.Repeat("1", 2000)
	if _, found, blocked := classifyCase(data); found || blocked == "" {
		t.Fatalf("found=%v blocked=%q", found, blocked)
	}
}

// A short prefix must block even when it holds no alias but the rest does.
func TestLFSClassifyBlobShortPrefixFailsClosed(t *testing.T) {
	full := pad("hello\n", 1100) + "version " + ptrV1 + "\n" + ptrBody
	for _, c := range []struct {
		prefix string
		size   int64
	}{
		{full[:300], int64(len(full))},  // truncated below min(size, 1024)
		{full, int64(len(full)) + 5},    // longer than declared
		{full[:1024] + "x", 5000},       // longer than the cap
		{"", 0},                         // empty blob
		{"version " + ptrV1 + "\n", -1}, // invalid size
	} {
		if _, found, blocked := classifyLFSBlob([]byte(c.prefix), c.size); found || blocked == "" {
			t.Fatalf("len=%d size=%d: found=%v blocked=%q", len(c.prefix), c.size, found, blocked)
		}
	}
}

func TestLFSInventoryConflicts(t *testing.T) {
	inv := lfsInventory{}
	if err := inv.add(lfsPointer{OID: ptrHex, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := inv.add(lfsPointer{OID: ptrHex, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := inv.add(lfsPointer{OID: ptrHex, Size: 2}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("error = %v", err)
	}
}

// Contrast with a real git-lfs: set GIT_LFS_TEST_BIN to run it; ordinary runs
// need no git-lfs. Soundness: confirmed implies git-lfs accepts; and anything
// git-lfs accepts is never classified as "none".
func TestLFSClassifyBlobAgainstGitLFS(t *testing.T) {
	bin := os.Getenv("GIT_LFS_TEST_BIN")
	if bin == "" {
		t.Skip("GIT_LFS_TEST_BIN not set")
	}
	for _, c := range lfsCases() {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(bin, "pointer", "--check", "--no-strict", "--stdin")
			cmd.Stdin = bytes.NewReader([]byte(c.data))
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			accepted := cmd.Run() == nil
			_, found, blocked := classifyCase(c.data)
			if found && !accepted {
				t.Fatal("confirmed a pointer that git-lfs rejects")
			}
			if accepted && !found && blocked == "" {
				t.Fatal("git-lfs accepts it but it was classified as none")
			}
			if c.found && !accepted {
				t.Fatal("fixture expected valid for git-lfs")
			}
		})
	}
}

func invNow(r *replicator, dir string) (lfsInventory, error) {
	refs, err := r.captureRefs(dir)
	if err != nil {
		return nil, err
	}
	inv, _, err := r.inventoryLFS(dir, refs)
	return inv, err
}

func lfsPtr(c byte, size string) []byte {
	return []byte("version " + ptrV1 + "\noid sha256:" + strings.Repeat(string(c), 64) + "\nsize " + size + "\n")
}

func lfsBlob(t *testing.T, dir string, data []byte) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir, cmd.Stdin = dir, bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestLFSInventoryHistoryTagsAndObsolete(t *testing.T) {
	big := bytes.Repeat([]byte{0xff, 'x'}, 40000) // binary, no LF, before the pointers
	dir := lfsWorkRepo(t, map[string][]byte{"big.bin": big, "a.ptr": lfsPtr('a', "10")})
	gitTest(t, dir, "rm", "-q", "a.ptr") // pointer survives only in history
	gitTest(t, dir, "commit", "-m", "drop")
	gitTest(t, dir, "update-ref", "refs/tags/b", lfsBlob(t, dir, lfsPtr('b', "20")))
	gitTest(t, dir, "update-ref", "refs/tags/c", lfsBlob(t, dir, lfsPtr('c', "30")))
	lfsBlob(t, dir, lfsPtr('d', "40")) // unreachable cached object

	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	inv, err := invNow(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := lfsInventory{strings.Repeat("a", 64): 10, strings.Repeat("b", 64): 20, strings.Repeat("c", 64): 30}
	if len(inv) != len(want) {
		t.Fatalf("inventory = %v", inv)
	}
	for oid, size := range want {
		if inv[oid] != size {
			t.Fatalf("oid %s size = %d, want %d", oid, inv[oid], size)
		}
	}
}

func TestLFSInventoryFailsClosed(t *testing.T) {
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	conflict := lfsWorkRepo(t, map[string][]byte{"x": lfsPtr('a', "10"), "y": lfsPtr('a', "11")})
	upper := lfsWorkRepo(t, map[string][]byte{"x": []byte("version " + ptrV1 + "\noid sha256:" + strings.ToUpper(ptrHex) + "\nsize 5\n")})
	for name, c := range map[string]struct{ dir, want string }{
		"conflicting sizes": {conflict, "conflicting Git LFS sizes"},
		"uppercase oid":     {upper, "invalid Git LFS pointer object id"},
	} {
		t.Run(name, func(t *testing.T) {
			inv, err := invNow(r, c.dir)
			if err == nil || inv != nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Git LFS blob ") {
				t.Fatalf("inventory = %v, err = %v", inv, err)
			}
		})
	}
}

func TestLFSInventoryCheckBlocksAmbiguous(t *testing.T) {
	dir := lfsWorkRepo(t, map[string][]byte{"x": []byte("version " + ptrV1 + "\nsize 5\noid " + ptrOID + "\n")})
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if inv, err := invNow(r, dir); err == nil || inv != nil {
		t.Fatalf("inventory = %v, err = %v", inv, err)
	}
}

func TestLFSInventoryIncludesOversizedHistoricalPointer(t *testing.T) {
	dir := lfsWorkRepo(t, map[string][]byte{
		"valid": lfsPtr('a', "10"),
		"old":   []byte(pad(string(lfsPtr('b', "20")), 1024)),
	})
	gitTest(t, dir, "rm", "-q", "old")
	gitTest(t, dir, "commit", "-m", "remove oversized pointer")
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	refs, err := r.captureRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	inv, big, err := r.inventoryLFS(dir, refs)
	if err != nil {
		t.Fatal(err)
	}
	if inv[strings.Repeat("a", 64)] != 10 || inv[strings.Repeat("b", 64)] != 20 || len(inv) != 2 {
		t.Fatalf("inventory = %v", inv)
	}
	if !big[strings.Repeat("b", 64)] || big[strings.Repeat("a", 64)] {
		t.Fatalf("big = %v, want only the oversized pointer", big)
	}
}

func TestLFSInventoryNoLFSAndErrors(t *testing.T) {
	dir := lfsWorkRepo(t, map[string][]byte{"a.txt": []byte("hello\n")})
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if inv, err := invNow(r, dir); err != nil || inv == nil || len(inv) != 0 {
		t.Fatalf("inventory = %v, err = %v", inv, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	if _, err := invNow(r, dir); err == nil {
		t.Fatal("cancelled inventory succeeded")
	}
	r.ctx = context.Background()
	if _, err := invNow(r, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing repository succeeded")
	}
}

func TestLFSInventoryUsesCapturedRefsOnly(t *testing.T) {
	dir := lfsWorkRepo(t, map[string][]byte{"a.ptr": lfsPtr('a', "10")})
	gitTest(t, dir, "update-ref", "refs/tags/blob", lfsBlob(t, dir, lfsPtr('b', "20")))                 // tag to a blob
	gitTest(t, dir, "-c", "user.name=T", "-c", "user.email=t@e", "tag", "-a", "-m", "x", "ann", "HEAD") // annotated
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	refs, err := r.captureRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ref := range refs {
		names[ref.Name] = true
	}
	if len(refs) != 3 || !names["refs/heads/main"] || !names["refs/tags/blob"] || !names["refs/tags/ann"] {
		t.Fatalf("refs = %+v", refs)
	}
	// Mutate after the capture: delete the blob tag, advance main, add a tag.
	gitTest(t, dir, "update-ref", "-d", "refs/tags/blob")
	if err := os.WriteFile(filepath.Join(dir, "c.ptr"), lfsPtr('c', "30"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "later")
	gitTest(t, dir, "update-ref", "refs/tags/later", lfsBlob(t, dir, lfsPtr('d', "40")))
	inv, _, err := r.inventoryLFS(dir, refs)
	if err != nil {
		t.Fatal(err)
	}
	want := lfsInventory{strings.Repeat("a", 64): 10, strings.Repeat("b", 64): 20}
	if len(inv) != 2 || inv[strings.Repeat("a", 64)] != 10 || inv[strings.Repeat("b", 64)] != 20 {
		t.Fatalf("inventory = %v, want %v", inv, want)
	}
}

func TestLFSInventoryEmptyCaptureIsEmpty(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	refs, err := r.captureRefs(dir)
	if err != nil || len(refs) != 0 {
		t.Fatalf("refs = %v, err = %v", refs, err)
	}
	// Pointer created after the empty capture must not appear ("--all" would find it).
	gitTest(t, dir, "update-ref", "refs/tags/late", lfsBlob(t, dir, lfsPtr('a', "10")))
	gitTest(t, dir, "update-ref", "refs/notes/x", lfsBlob(t, dir, lfsPtr('b', "10")))
	inv, _, err := r.inventoryLFS(dir, refs)
	if err != nil || inv == nil || len(inv) != 0 {
		t.Fatalf("inventory = %v, err = %v", inv, err)
	}
}

func TestLFSCaptureRefsErrors(t *testing.T) {
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if _, err := r.captureRefs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing repository succeeded")
	}
	dir := lfsWorkRepo(t, map[string][]byte{"a.txt": []byte("x")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	if _, err := r.captureRefs(dir); err == nil {
		t.Fatal("cancelled capture succeeded")
	}
	// Captured tips that no longer exist make the scan fail, never pass silently.
	r.ctx = context.Background()
	bad := []gitRef{{Name: "refs/heads/x", ID: strings.Repeat("1", 40)}}
	if inv, _, err := r.inventoryLFS(dir, bad); err == nil || inv != nil {
		t.Fatalf("inventory = %v, err = %v", inv, err)
	}
}

func TestLFSInventoryCanceledEmptyCapture(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "--bare")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &replicator{ctx: ctx, timeout: time.Minute}
	if inv, _, err := r.inventoryLFS(dir, nil); err == nil || inv != nil {
		t.Fatalf("inventory = %v, err = %v", inv, err)
	}
}

func TestLFSInventoryRejectsGraftEnv(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "--bare")
	t.Setenv("GIT_GRAFT_FILE", "ignored")
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if _, _, err := r.inventoryLFS(dir, nil); err == nil || !strings.Contains(err.Error(), "GIT_GRAFT_FILE") {
		t.Fatalf("err = %v", err)
	}
}

func TestLFSInventoryRejectsShallowFile(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "--bare")
	if err := os.WriteFile(filepath.Join(dir, "shallow"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &replicator{ctx: context.Background(), timeout: time.Minute}
	if inv, _, err := r.inventoryLFS(dir, nil); err == nil || inv != nil || !strings.Contains(err.Error(), "shallow") {
		t.Fatalf("inventory = %v, err = %v", inv, err)
	}
}
