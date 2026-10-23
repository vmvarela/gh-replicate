package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: gh replicate SOURCE TARGET")
	}
	source, target := args[0], args[1]
	if !validRepo(source) || !validRepo(target) {
		return errors.New("repositories must use OWNER/REPO format")
	}
	if err := ensureRepository(source); err != nil {
		return err
	}
	if err := ensureRepository(target); err != nil {
		return err
	}
	return syncGit(source, target)
}

func validRepo(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && p[0] != "" && p[1] != ""
}

func ensureRepository(repo string) error {
	if err := command("", "gh", "api", "repos/"+repo, "--silent"); err != nil {
		return fmt.Errorf("access repository %s: %w", repo, err)
	}
	return nil
}

func syncGit(source, target string) error {
	dir, err := os.MkdirTemp("", "gh-replicate-*")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)

	gitDir := filepath.Join(dir, "repo.git")
	if err := command("", "git", "clone", "--bare", "https://github.com/"+source+".git", gitDir); err != nil {
		return fmt.Errorf("clone source: %w", err)
	}
	if err := command(gitDir, "git", "push", "--force", "https://github.com/"+target+".git", "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil {
		return fmt.Errorf("push refs: %w", err)
	}
	return nil
}

func command(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
