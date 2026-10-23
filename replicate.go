package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type repository struct {
	Description string `json:"description"`
	Homepage string `json:"homepage"`
}
type label struct {
	Name string `json:"name"`
	Color string `json:"color"`
	Description string `json:"description"`
}

func run(args []string) error {
	if len(args) != 2 { return errors.New("usage: gh replicate SOURCE TARGET") }
	source, target := args[0], args[1]
	if !validRepo(source) || !validRepo(target) { return errors.New("repositories must use OWNER/REPO format") }
	if err := ensureRepository(source); err != nil { return err }
	if err := ensureRepository(target); err != nil { return err }
	if err := syncGit(source, target); err != nil { return err }
	return syncMetadata(source, target)
}
func validRepo(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && p[0] != "" && p[1] != ""
}
func ensureRepository(repo string) error {
	if _, err := ghAPI("GET", "repos/"+repo, nil); err != nil { return fmt.Errorf("access repository %s: %w", repo, err) }
	return nil
}
func syncGit(source, target string) error {
	dir, err := os.MkdirTemp("", "gh-replicate-*")
	if err != nil { return fmt.Errorf("create temporary directory: %w", err) }
	defer os.RemoveAll(dir)
	gitDir := filepath.Join(dir, "repo.git")
	if err := command("", "gh", "repo", "clone", source, gitDir, "--", "--bare"); err != nil { return fmt.Errorf("clone source: %w", err) }
	if err := command(gitDir, "git", "remote", "add", "replicate-target", "https://github.com/"+target+".git"); err != nil { return fmt.Errorf("add destination remote: %w", err) }
	if err := command(gitDir, "gh", "auth", "setup-git"); err != nil { return fmt.Errorf("configure Git authentication: %w", err) }
	if err := command(gitDir, "git", "push", "--force", "replicate-target", "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil { return fmt.Errorf("push refs: %w", err) }
	return nil
}
func syncMetadata(source, target string) error {
	var repo repository
	if err := getJSON("repos/"+source, &repo); err != nil { return fmt.Errorf("read repository metadata: %w", err) }
	if _, err := ghAPI("PATCH", "repos/"+target, map[string]any{"description": repo.Description, "homepage": repo.Homepage}); err != nil { return fmt.Errorf("update repository metadata: %w", err) }
	var topics struct { Names []string `json:"names"` }
	if err := getJSON("repos/"+source+"/topics", &topics); err != nil { return fmt.Errorf("read topics: %w", err) }
	if _, err := ghAPI("PUT", "repos/"+target+"/topics", topics); err != nil { return fmt.Errorf("update topics: %w", err) }
	var labels []label
	if err := getJSON("repos/"+source+"/labels?per_page=100", &labels); err != nil { return fmt.Errorf("read labels: %w", err) }
	for _, l := range labels {
		path := "repos/"+target+"/labels/"+url.PathEscape(l.Name)
		if _, err := ghAPI("PATCH", path, l); err != nil {
			if _, createErr := ghAPI("POST", "repos/"+target+"/labels", l); createErr != nil { return fmt.Errorf("sync label %q: %w", l.Name, createErr) }
		}
	}
	return nil
}
func getJSON(path string, out any) error {
	data, err := ghAPI("GET", path, nil)
	if err != nil { return err }
	return json.Unmarshal(data, out)
}
func ghAPI(method, path string, body any) ([]byte, error) {
	args := []string{"api", "--method", method, path}
	var stdin []byte
	if body != nil {
		var err error
		stdin, err = json.Marshal(body)
		if err != nil { return nil, err }
		args = append(args, "--input", "-")
	}
	cmd := exec.Command("gh", args...)
	if stdin != nil { cmd.Stdin = bytes.NewReader(stdin) }
	out, err := cmd.CombinedOutput()
	if err != nil { return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err) }
	return out, nil
}
func command(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil { return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err) }
	return nil
}
