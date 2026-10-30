//go:build windows

package main

import (
	"context"
	"os/exec"
)

// lfsTreeCancel is false on Windows: newLFSCmd kills only the direct child, so
// a cancellation can leave git-lfs descendants running during cleanup. LFS
// transfer is refused here until a Job Object makes cancellation kill the tree.
const lfsTreeCancel = false

// newLFSCmd on Windows kills only the direct child.
// shortcut: git-lfs descendants may survive a cancellation on Windows; add a
// Job Object before enabling LFS transfer there.
func newLFSCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...) // #nosec G204 -- name is the resolved absolute git binary, argv only
}
