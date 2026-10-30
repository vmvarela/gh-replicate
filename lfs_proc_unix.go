//go:build unix

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// newLFSCmd starts the command in its own process group so a cancellation kills
// the whole tree, including git-lfs descendants that hold the cache lock.
func newLFSCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- name is the resolved absolute git binary, argv only
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}
