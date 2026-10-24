package main

import (
	"fmt"
	"os"
)

// Keep the lock file: unlinking it could let processes lock different inodes.
// Closing the handle (also on process termination) releases the OS lock.
func lockCache(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open cache lock %s: %w", path, err)
	}
	if err := tryCacheLock(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot lock source cache %s (another replication may be running): %w", path, err)
	}
	return f, nil
}
