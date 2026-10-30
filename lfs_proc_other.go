//go:build !unix && !windows

package main

import (
	"context"
	"os/exec"
)

// newLFSCmd on unsupported platforms (e.g. plan9) kills only the direct child.
// shortcut: git-lfs descendants may survive a cancellation here; LFS transfer is
// not exercised on these platforms.
func newLFSCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...) // #nosec G204 -- name is the resolved absolute git binary, argv only
}
