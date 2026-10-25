//go:build !unix && !windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

// The persistent cache needs an exclusive lock. Platforms without one fail
// loudly instead of replicating concurrently into the same cache.
func openCacheLock(path string) (*os.File, error) {
	return nil, errors.New("cache locking is not supported on this platform")
}

func inheritCacheLock(cmd *exec.Cmd, f *os.File) {}
