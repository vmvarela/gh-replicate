//go:build unix

package main

import (
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

func openCacheLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600) // #nosec G304 -- callers (journal staged-v1 and lockCache git-v1) pass <user cache>/gh-replicate/<dir>/<SHA-256 hex>.lock; hashed input cannot add path segments
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close() // #nosec G104 -- f.Close on the failure path; the Flock error is what is returned
		return nil, err
	}
	return f, nil
}

func inheritCacheLock(cmd *exec.Cmd, f *os.File) {
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
}
