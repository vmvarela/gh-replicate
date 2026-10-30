//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLFSCmdCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	cmd := newLFSCmd(ctx, "sh", "-c", "sleep 30 & echo $! > "+pidfile+"; wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(pidfile); err == nil && len(b) > 0 {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				pid = p
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("child pid never recorded")
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected cancellation error")
	}
	// SIGKILL to the process group leaves no descendant behind.
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil || err != syscall.ESRCH {
		t.Fatalf("child %d still alive (err=%v)", pid, err)
	}
}
