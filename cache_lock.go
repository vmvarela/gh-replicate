package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Never unlink the lock file: all users must lock the same file.
func lockCache(path string) (*os.File, error) {
	f, err := openCacheLock(path)
	if err != nil {
		return nil, fmt.Errorf("cannot lock source cache %s (another replication may be running): %w", path, err)
	}
	return f, nil
}

// Git retains a reference to the lock if the parent is killed mid-command.
func (r *replicator) gitCommand(ctx context.Context, token, name string, args ...string) *exec.Cmd {
	cmd := gitCommand(ctx, token, name, args...)
	if r.cacheLock != nil {
		inheritCacheLock(cmd, r.cacheLock)
	}
	return cmd
}
