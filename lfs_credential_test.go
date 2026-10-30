package main

import (
	"context"
	"strings"
	"testing"
)

// TestLFSSideCredentialsAreSeparated checks that the command built for the
// source side carries only the source token and source endpoint, and the target
// side only the target token and endpoint. lfsCmd only builds the command (it
// does not run), so this is deterministic and needs no git-lfs.
func TestLFSSideCredentialsAreSeparated(t *testing.T) {
	r := &replicator{
		source: side{token: "source-token"},
		target: side{token: "target-token"},
		ctx:    context.Background(),
	}
	a := newAux(t)
	src := r.lfsCmd(context.Background(), a, r.source, "owner/source", "lfs", "fetch")
	dst := r.lfsCmd(context.Background(), a, r.target, "owner/target", "lfs", "push")

	if got := envValue(src.Env, "GH_TOKEN"); got != "source-token" {
		t.Fatalf("source GH_TOKEN = %q, want source-token", got)
	}
	if got := envValue(dst.Env, "GH_TOKEN"); got != "target-token" {
		t.Fatalf("target GH_TOKEN = %q, want target-token", got)
	}
	srcArgs := strings.Join(src.Args, " ")
	dstArgs := strings.Join(dst.Args, " ")
	if !strings.Contains(srcArgs, "lfs.url=https://github.com/owner/source.git/info/lfs") ||
		!strings.Contains(srcArgs, "lfs.pushurl=https://github.com/owner/source.git/info/lfs") {
		t.Fatalf("source endpoint not fully pinned: %s", srcArgs)
	}
	if !strings.Contains(dstArgs, "lfs.url=https://github.com/owner/target.git/info/lfs") ||
		!strings.Contains(dstArgs, "lfs.pushurl=https://github.com/owner/target.git/info/lfs") {
		t.Fatalf("target endpoint not fully pinned: %s", dstArgs)
	}
	// The isolated environment must not inherit another GitHub token.
	if envValue(src.Env, "GITHUB_TOKEN") != "" || envValue(dst.Env, "GITHUB_TOKEN") != "" {
		t.Fatalf("inherited GITHUB_TOKEN leaked: src=%q dst=%q", envValue(src.Env, "GITHUB_TOKEN"), envValue(dst.Env, "GITHUB_TOKEN"))
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 && kv[:i] == key {
			return kv[i+1:]
		}
	}
	return ""
}
