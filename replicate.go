package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

type repository struct {
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
}

type label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type release struct {
	ID              int64   `json:"id"`
	TagName         string  `json:"tag_name"`
	TargetCommitish string  `json:"target_commitish"`
	Name            string  `json:"name"`
	Body            string  `json:"body"`
	Draft           bool    `json:"draft"`
	Prerelease      bool    `json:"prerelease"`
	Assets          []asset `json:"assets"`
}

type asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"url"`
}

var client *api.RESTClient
var httpClient *http.Client
var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: gh replicate SOURCE TARGET")
	}
	var err error
	client, err = api.DefaultRESTClient()
	if err != nil {
		return fmt.Errorf("create GitHub REST client: %w", err)
	}
	httpClient, err = api.DefaultHTTPClient()
	if err != nil {
		return fmt.Errorf("create GitHub HTTP client: %w", err)
	}

	source, target := args[0], args[1]
	if strings.EqualFold(source, target) { return errors.New("source and target must be different repositories") }
	if !validRepo(source) || !validRepo(target) {
		return errors.New("repositories must use OWNER/REPO format")
	}
	fmt.Fprintln(os.Stderr, "Checking repositories...")
	if err := ensureRepository(source); err != nil { return err }
	if err := ensureRepository(target); err != nil { return err }
	fmt.Fprintln(os.Stderr, "Replicating Git refs...")
	if err := syncGit(source, target); err != nil { return err }
	fmt.Fprintln(os.Stderr, "Replicating repository metadata and labels...")
	if err := syncMetadata(source, target); err != nil { return err }
	fmt.Fprintln(os.Stderr, "Replicating releases...")
	if err := syncReleases(source, target); err != nil { return err }
	fmt.Fprintln(os.Stderr, "Replication complete.")
	return nil
}

func validRepo(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && repoPart.MatchString(p[0]) && repoPart.MatchString(p[1]) && p[0] != "." && p[0] != ".." && p[1] != "." && p[1] != ".."
}

func ensureRepository(repo string) error {
	var r repository
	if err := client.Get("repos/"+repo, &r); err != nil {
		return fmt.Errorf("access repository %s: %w", repo, err)
	}
	return nil
}

func syncGit(source, target string) error {
	dir, err := os.MkdirTemp("", "gh-replicate-*")
	if err != nil { return fmt.Errorf("create temporary directory: %w", err) }
	defer os.RemoveAll(dir)

	gitDir := filepath.Join(dir, "repo.git")
	fmt.Fprintf(os.Stderr, "  Cloning %s...\n", source)
	if err := command("", "gh", "repo", "clone", source, gitDir, "--", "--bare"); err != nil {
		return fmt.Errorf("clone source: %w", err)
	}
	if err := command(gitDir, "git", "remote", "add", "replicate-target", "https://github.com/"+target+".git"); err != nil {
		return fmt.Errorf("add destination remote: %w", err)
	}
	fmt.Fprintln(os.Stderr, "  Configuring Git authentication...")
	if err := command(gitDir, "gh", "auth", "setup-git"); err != nil {
		return fmt.Errorf("configure Git authentication: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Pushing branches and tags to %s...\n", target)
	if err := command(gitDir, "git", "push", "--force", "replicate-target",
		"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil {
		return fmt.Errorf("push refs: %w", err)
	}
	return nil
}

func syncMetadata(source, target string) error {
	var repo repository
	if err := client.Get("repos/"+source, &repo); err != nil {
		return fmt.Errorf("read repository metadata: %w", err)
	}
	if err := client.Patch("repos/"+target, jsonBody(map[string]any{
		"description": repo.Description,
		"homepage": repo.Homepage,
	}), nil); err != nil {
		return fmt.Errorf("update repository metadata: %w", err)
	}

	var topics struct { Names []string `json:"names"` }
	if err := client.Get("repos/"+source+"/topics", &topics); err != nil {
		return fmt.Errorf("read topics: %w", err)
	}
	var targetTopics struct { Names []string `json:"names"` }
	if err := client.Get("repos/"+target+"/topics", &targetTopics); err != nil { return fmt.Errorf("read destination topics: %w", err) }
	seen := make(map[string]bool)
	for _, name := range topics.Names { seen[name] = true }
	for _, name := range targetTopics.Names { if !seen[name] { topics.Names = append(topics.Names, name); seen[name] = true } }
	if topics.Names == nil { topics.Names = []string{} }
	if err := client.Put("repos/"+target+"/topics", jsonBody(topics), nil); err != nil {
		return fmt.Errorf("update topics: %w", err)
	}

	labels, err := getAll[label]("repos/"+source+"/labels?per_page=100")
	if err != nil { return fmt.Errorf("read labels: %w", err) }
	for _, l := range labels {
		path := "repos/" + target + "/labels/" + url.PathEscape(l.Name)
		if err := client.Patch(path, jsonBody(l), nil); err != nil {
			var apiErr *api.HTTPError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound { return fmt.Errorf("update label %q: %w", l.Name, err) }
			if err := client.Post("repos/"+target+"/labels", jsonBody(l), nil); err != nil {
				return fmt.Errorf("sync label %q: %w", l.Name, err)
			}
		}
	}
	return nil
}

func syncReleases(source, target string) error {
	sourceReleases, err := getAll[release]("repos/"+source+"/releases?per_page=100")
	if err != nil { return fmt.Errorf("read releases: %w", err) }
	targetReleases, err := getAll[release]("repos/"+target+"/releases?per_page=100")
	if err != nil { return fmt.Errorf("read destination releases: %w", err) }

	byTag := make(map[string]release, len(targetReleases))
	for _, r := range targetReleases { byTag[r.TagName] = r }

	for _, src := range sourceReleases {
		fmt.Fprintf(os.Stderr, "  Release %s...\n", src.TagName)
		dst, exists := byTag[src.TagName]
		payload := map[string]any{
			"tag_name": src.TagName, "target_commitish": src.TargetCommitish,
			"name": src.Name, "body": src.Body, "draft": src.Draft, "prerelease": src.Prerelease,
		}
		if exists {
			if err := client.Patch(fmt.Sprintf("repos/%s/releases/%d", target, dst.ID), jsonBody(payload), &dst); err != nil {
				return fmt.Errorf("update release %s: %w", src.TagName, err)
			}
		} else {
			if err := client.Post("repos/"+target+"/releases", jsonBody(payload), &dst); err != nil {
				return fmt.Errorf("create release %s: %w", src.TagName, err)
			}
		}
		if err := syncAssets(target, src, dst); err != nil { return err }
	}
	return nil
}

func syncAssets(target string, src, dst release) error {
	existing := make(map[string]asset, len(dst.Assets))
	for _, a := range dst.Assets { existing[a.Name] = a }

	for _, a := range src.Assets {
		if d, ok := existing[a.Name]; ok && sameAsset(a, d) {
			fmt.Fprintf(os.Stderr, "    Asset %s unchanged.\n", a.Name)
			continue
		}
		fmt.Fprintf(os.Stderr, "    Replicating asset %s...\n", a.Name)

		tmp, err := os.CreateTemp("", "gh-replicate-asset-*")
		if err != nil { return fmt.Errorf("create temporary asset file: %w", err) }
		name := tmp.Name()
		defer os.Remove(name)

		if err := downloadAsset(a.URL, tmp); err != nil {
			tmp.Close()
			return fmt.Errorf("download asset %s: %w", a.Name, err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			tmp.Close()
			return fmt.Errorf("rewind asset %s: %w", a.Name, err)
		}
		if d, ok := existing[a.Name]; ok {
			if err := client.Delete(fmt.Sprintf("repos/%s/releases/assets/%d", target, d.ID), nil); err != nil {
				tmp.Close()
				return fmt.Errorf("replace asset %s: %w", a.Name, err)
			}
		}
		if err := uploadAsset(target, dst.ID, a.Name, tmp); err != nil {
			tmp.Close()
			return fmt.Errorf("upload asset %s: %w", a.Name, err)
		}
		if err := tmp.Close(); err != nil { return fmt.Errorf("close asset %s: %w", a.Name, err) }
		os.Remove(name)
	}
	return nil
}

func downloadAsset(path string, dst io.Writer) error {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil { return err }
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := httpClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { return fmt.Errorf("asset request failed: HTTP %d", resp.StatusCode) }
	_, err = io.Copy(dst, resp.Body)
	return err
}

func uploadAsset(target string, releaseID int64, name string, src *os.File) error {
	stat, err := src.Stat()
	if err != nil { return err }

	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" { contentType = "application/octet-stream" }

	path := fmt.Sprintf("https://uploads.github.com/repos/%s/releases/%d/assets?name=%s", target, releaseID, url.QueryEscape(name))
	req, err := http.NewRequest(http.MethodPost, path, src)
	if err != nil { return err }
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = stat.Size()

	resp, err := httpClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { return fmt.Errorf("asset request failed: HTTP %d", resp.StatusCode) }
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func sameAsset(a, b asset) bool {
	if a.Digest != "" && b.Digest != "" { return a.Digest == b.Digest }
	return false
}

func getAll[T any](path string) ([]T, error) {
	var all []T
	for {
		resp, err := client.Request(http.MethodGet, path, nil)
		if err != nil { return nil, err }
		var page []T
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil { return nil, err }
		all = append(all, page...)

		match := nextLink.FindStringSubmatch(resp.Header.Get("Link"))
		if len(match) == 0 { return all, nil }
		path = match[1]
	}
}

func jsonBody(v any) io.Reader {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(v)
	return &b
}

func command(dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s timed out after 5m", name)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
