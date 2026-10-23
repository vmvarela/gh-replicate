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

type release struct {
	ID int64 `json:"id"`
	TagName string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Name string `json:"name"`
	Body string `json:"body"`
	Draft bool `json:"draft"`
	Prerelease bool `json:"prerelease"`
	Assets []asset `json:"assets"`
}
type asset struct {
	Name string `json:"name"`
	Size int64 `json:"size"`
	Digest string `json:"digest"`
	URL string `json:"url"`
}
type label struct {
	Name string `json:"name"`
	Color string `json:"color"`
	Description string `json:"description"`
}

var client *api.RESTClient\n\nfunc run(args []string) error {\n\tvar err error\n\tclient, err = api.DefaultRESTClient()\n\tif err != nil { return fmt.Errorf("create GitHub client: %w", err) }
	if len(args) != 2 { return errors.New("usage: gh replicate SOURCE TARGET") }
	source, target := args[0], args[1]
	if !validRepo(source) || !validRepo(target) { return errors.New("repositories must use OWNER/REPO format") }
	if err := ensureRepository(source); err != nil { return err }
	if err := ensureRepository(target); err != nil { return err }
	if err := syncGit(source, target); err != nil { return err }
	if err := syncMetadata(source, target); err != nil { return err }\n\treturn syncReleases(source, target)
}
func validRepo(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && p[0] != "" && p[1] != ""
}
func ensureRepository(repo string) error {
	var r repository
	if err := client.Get("repos/"+repo, &r); err != nil { return fmt.Errorf("access repository %s: %w", repo, err) }
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
	if err := client.Patch("repos/"+target, jsonBody(map[string]any{"description": repo.Description, "homepage": repo.Homepage}), nil); err != nil { return fmt.Errorf("update repository metadata: %w", err) }
	var topics struct { Names []string `json:"names"` }
	if err := getJSON("repos/"+source+"/topics", &topics); err != nil { return fmt.Errorf("read topics: %w", err) }
	if err := client.Put("repos/"+target+"/topics", jsonBody(topics), nil); err != nil { return fmt.Errorf("update topics: %w", err) }
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

func syncReleases(source, target string) error {
	var sourceReleases, targetReleases []release
	if err := getJSON("repos/"+source+"/releases?per_page=100", &sourceReleases); err != nil { return fmt.Errorf("read releases: %w", err) }
	if err := getJSON("repos/"+target+"/releases?per_page=100", &targetReleases); err != nil { return fmt.Errorf("read destination releases: %w", err) }
	byTag := make(map[string]release, len(targetReleases))
	for _, r := range targetReleases { byTag[r.TagName] = r }

	for _, src := range sourceReleases {
		dst, exists := byTag[src.TagName]
		payload := map[string]any{"tag_name": src.TagName, "target_commitish": src.TargetCommitish, "name": src.Name, "body": src.Body, "draft": src.Draft, "prerelease": src.Prerelease}
		if exists {
			if err := client.Patch(fmt.Sprintf("repos/%s/releases/%d", target, dst.ID), jsonBody(payload), &dst); err != nil { return fmt.Errorf("update release %s: %w", src.TagName, err) }
		} else {
			if err := client.Post("repos/"+target+"/releases", jsonBody(payload), &dst); err != nil { return fmt.Errorf("create release %s: %w", src.TagName, err) }
		}
		if err := syncAssets(source, target, src, dst); err != nil { return err }
	}
	return nil
}

func syncAssets(source, target string, src, dst release) error {
	existing := make(map[string]asset, len(dst.Assets))
	for _, a := range dst.Assets { existing[a.Name] = a }
	for _, a := range src.Assets {
		if d, ok := existing[a.Name]; ok && sameAsset(a, d) { continue }
		if d, ok := existing[a.Name]; ok {
			if err := client.Delete(fmt.Sprintf("repos/%s/releases/assets/%d", target, d.ID), nil); err != nil { return fmt.Errorf("replace asset %s: %w", a.Name, err) }
		}
		tmp, err := os.CreateTemp("", "gh-replicate-asset-*")
		if err != nil { return err }
		name := tmp.Name()
		tmp.Close()
		cmd := exec.Command("gh", "api", "-H", "Accept: application/octet-stream", a.URL)
		data, err := cmd.Output()
		if err != nil { os.Remove(name); return fmt.Errorf("download asset %s: %w", a.Name, err) }
		if err := os.WriteFile(name, data, 0600); err != nil { os.Remove(name); return err }
		err = command("", "gh", "release", "upload", src.TagName, name+"#"+a.Name, "--repo", target)
		os.Remove(name)
		if err != nil { return fmt.Errorf("upload asset %s: %w", a.Name, err) }
	}
	return nil
}

