package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The journal records a staged asset name before uploading it, so a later run
// can prove an asset with that name came from this tool. Content alone cannot
// prove ownership: a user can upload identical bytes under the same name.
type journalEntry struct {
	Staged string `json:"staged"`
}

type assetJournal struct {
	path    string
	entries map[string]journalEntry
	lock    *os.File
}

func openJournal(cacheDir, target string) (*assetJournal, error) {
	if cacheDir == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("locate cache directory: %w", err)
		}
		cacheDir = dir
	}
	dir := filepath.Join(cacheDir, "gh-replicate", "staged-v1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(target))))
	lock, err := openCacheLock(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("cannot lock journal %s (another replication may be running): %w", path, err)
	}
	j := &assetJournal{path: path, entries: map[string]journalEntry{}, lock: lock}
	data, err := os.ReadFile(path) // #nosec G304 -- cacheDir comes from the caller or os.UserCacheDir; the file name is a SHA-256 hex of target, so target cannot add path segments
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		lock.Close() // #nosec G104 -- cleanup on error path; the original error is returned
		return nil, fmt.Errorf("read journal: %w", err)
	default:
		// Only a missing file means "no entries yet"; anything unreadable must
		// stop the run rather than silently forget which assets are ours.
		if err := json.Unmarshal(data, &j.entries); err != nil {
			lock.Close() // #nosec G104 -- cleanup on error path; the original error is returned
			return nil, fmt.Errorf("parse journal %s: %w (remove it to start over)", path, err)
		}
		if j.entries == nil {
			lock.Close() // #nosec G104 -- cleanup on error path; the original error is returned
			return nil, fmt.Errorf("parse journal %s: no entries object (remove it to start over)", path)
		}
		for k, e := range j.entries {
			if k == "" || e.Staged == "" {
				lock.Close() // #nosec G104 -- cleanup on error path; the original error is returned
				return nil, fmt.Errorf("parse journal %s: entry %q has no staged name (remove it to start over)", path, k)
			}
		}
	}
	return j, nil
}

func (j *assetJournal) Close() error { return j.lock.Close() }

func journalKey(releaseID int64, name string) string {
	return fmt.Sprintf("%d/%s", releaseID, name)
}

func (j *assetJournal) get(releaseID int64, name string) (journalEntry, bool) {
	e, ok := j.entries[journalKey(releaseID, name)]
	return e, ok
}

func (j *assetJournal) set(releaseID int64, name string, e journalEntry) error {
	j.entries[journalKey(releaseID, name)] = e
	return j.save()
}

func (j *assetJournal) del(releaseID int64, name string) error {
	if _, ok := j.entries[journalKey(releaseID, name)]; !ok {
		return nil
	}
	delete(j.entries, journalKey(releaseID, name))
	return j.save()
}

// save replaces the journal atomically, so a crash cannot leave it half-written.
func (j *assetJournal) save() error {
	data, err := json.Marshal(j.entries)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".journal-*")
	if err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() // #nosec G104 -- cleanup on error path; the write/sync error is returned
		return fmt.Errorf("write journal: %w", err)
	}
	// Sync before the rename: the entry must survive a crash after the upload
	// it authorises, or the staged copy becomes unprovable.
	if err := tmp.Sync(); err != nil {
		tmp.Close() // #nosec G104 -- cleanup on error path; the write/sync error is returned
		return fmt.Errorf("write journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	if err := os.Rename(tmp.Name(), j.path); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(j.path)); err == nil {
		dir.Sync()  // #nosec G104 -- best-effort directory fsync after a successful rename; unsupported on some platforms
		dir.Close() // #nosec G104 -- closes the read-only directory handle opened for the best-effort fsync; no data is written through it
	}
	return nil
}
