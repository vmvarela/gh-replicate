package main

import (
	"fmt"
	"os"
)

// Never unlink the lock file: all users must lock the same file.
func lockCache(path string) (*os.File, error) {
	f, err := openCacheLock(path)
	if err != nil {
		return nil, fmt.Errorf("cannot lock source cache %s (another replication may be running): %w", path, err)
	}
	return f, nil
}
