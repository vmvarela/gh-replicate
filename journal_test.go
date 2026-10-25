package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalPath(cacheDir, target string) string {
	return filepath.Join(cacheDir, "gh-replicate", "staged-v1",
		fmt.Sprintf("%x.json", sha256.Sum256([]byte(target))))
}

func TestJournalStoresAndClearsEntries(t *testing.T) {
	dir := t.TempDir()
	j, err := openJournal(dir, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.get(1, "binary"); ok {
		t.Fatal("new journal already has an entry")
	}
	if err := j.set(1, "binary", journalEntry{Staged: "binary.gh-replicate-00112233"}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	j, err = openJournal(dir, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := j.get(1, "binary")
	if !ok || entry.Staged != "binary.gh-replicate-00112233" {
		t.Fatalf("entry %+v present=%v, want the saved staged name", entry, ok)
	}
	if err := j.del(1, "binary"); err != nil {
		t.Fatal(err)
	}
	j.Close()

	j, err = openJournal(dir, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, ok := j.get(1, "binary"); ok {
		t.Fatal("deleted entry came back")
	}
}

// A journal that cannot be read must stop the run: guessing which staged
// assets are ours is worse than stopping.
func TestJournalRejectsUnusableContent(t *testing.T) {
	for name, content := range map[string]string{
		"empty":          "",
		"null":           "null",
		"corrupt":        "{",
		"missing staged": `{"1/binary":{"staged":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := journalPath(dir, "a/b")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := openJournal(dir, "a/b"); err == nil || !strings.Contains(err.Error(), "journal") {
				t.Fatalf("expected a journal error, got %v", err)
			}
		})
	}
}
