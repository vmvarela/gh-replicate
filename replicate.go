package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/auth"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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

type replicator struct {
	client     *api.RESTClient
	httpClient *http.Client
	ctx        context.Context
	timeout    time.Duration
	dryRun     bool
}

type plan struct {
	repository     repository
	topics         []string
	labels         []label
	sourceReleases []release
	targetReleases []release
	latest         string
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func run(args []string) error {
	flags := flag.NewFlagSet("gh replicate", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "inspect replication without changing the destination")
	timeout := flags.Duration("timeout", 30*time.Minute, "timeout per Git command or HTTP request")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: gh replicate [--dry-run] [--timeout 30m] SOURCE TARGET")
	}
	if *timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	source, target := flags.Arg(0), flags.Arg(1)
	if !validRepo(source) || !validRepo(target) {
		return errors.New("repositories must use OWNER/REPO format")
	}
	if strings.EqualFold(source, target) {
		return errors.New("source and target must be different repositories")
	}
	host, _ := auth.DefaultHost()
	if host != "github.com" {
		return errors.New("only github.com is supported; set GH_HOST=github.com")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	opts := api.ClientOptions{Host: "github.com", Timeout: *timeout, Transport: retryTransport{base: http.DefaultTransport}}
	client, err := api.NewRESTClient(opts)
	if err != nil {
		return err
	}
	httpClient, err := api.NewHTTPClient(opts)
	if err != nil {
		return err
	}
	r := &replicator{client: client, httpClient: httpClient, ctx: ctx, timeout: *timeout, dryRun: *dryRun}
	fmt.Fprintf(os.Stderr, "Source: %s -> Target: %s (matching refs will be overwritten)\n", source, target)
	p, err := r.preflight(source, target)
	if err != nil {
		return err
	}
	if err := r.syncGit(source, target); err != nil {
		return err
	}
	if err := r.syncMetadata(target, p); err != nil {
		return err
	}
	if err := r.syncReleases(target, p); err != nil {
		return err
	}
	if r.dryRun {
		fmt.Fprintln(os.Stderr, "Dry run complete; no destination changes made.")
	} else {
		fmt.Fprintln(os.Stderr, "Replication complete.")
	}
	return nil
}

// call is the only JSON write path; dry runs never send mutating HTTP requests.
func (r *replicator) call(method, path string, body io.Reader, out any) error {
	if r.dryRun && method != http.MethodGet {
		fmt.Fprintf(os.Stderr, "  Would %s %s\n", method, path)
		return nil
	}
	return r.client.DoWithContext(r.ctx, method, path, body, out)
}

func (r *replicator) preflight(source, target string) (*plan, error) {
	p := &plan{}
	if err := r.call("GET", "repos/"+source, nil, &p.repository); err != nil {
		return nil, err
	}
	var dst repository
	if err := r.call("GET", "repos/"+target, nil, &dst); err != nil {
		return nil, err
	}
	var srcTopics, dstTopics struct {
		Names []string `json:"names"`
	}
	if err := r.call("GET", "repos/"+source+"/topics", nil, &srcTopics); err != nil {
		return nil, err
	}
	if err := r.call("GET", "repos/"+target+"/topics", nil, &dstTopics); err != nil {
		return nil, err
	}
	var err error
	p.topics, err = unionTopics(srcTopics.Names, dstTopics.Names)
	if err != nil {
		return nil, err
	}
	p.labels, err = getAll[label](r, "repos/"+source+"/labels?per_page=100")
	if err != nil {
		return nil, err
	}
	p.sourceReleases, err = getAll[release](r, "repos/"+source+"/releases?per_page=100")
	if err != nil {
		return nil, err
	}
	p.targetReleases, err = getAll[release](r, "repos/"+target+"/releases?per_page=100")
	if err != nil {
		return nil, err
	}
	if err := uniqueTags(p.sourceReleases); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	if err := uniqueTags(p.targetReleases); err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	var latest release
	if err := r.call("GET", "repos/"+source+"/releases/latest", nil, &latest); err != nil {
		if !isNotFound(err) {
			return nil, err
		}
	} else {
		found := false
		for _, rel := range p.sourceReleases {
			if rel.ID == latest.ID && !rel.Draft && !rel.Prerelease {
				found = true
			}
		}
		if !found {
			return nil, errors.New("Latest changed during preflight; rerun to obtain a consistent snapshot")
		}
		p.latest = latest.TagName
	}
	return p, nil
}

func validRepo(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && repoPart.MatchString(p[0]) && repoPart.MatchString(p[1]) && p[0] != "." && p[0] != ".." && p[1] != "." && p[1] != ".."
}

func (r *replicator) syncGit(source, target string) error {
	dir, err := os.MkdirTemp("", "gh-replicate-*")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)

	gitDir := filepath.Join(dir, "repo.git")
	fmt.Fprintf(os.Stderr, "  Cloning %s...\n", source)
	if err := r.command("", "git", "clone", "--bare", "https://github.com/"+source+".git", gitDir); err != nil {
		return fmt.Errorf("clone source: %w", err)
	}
	if err := r.command(gitDir, "git", "remote", "add", "replicate-target", "https://github.com/"+target+".git"); err != nil {
		return fmt.Errorf("add destination remote: %w", err)
	}
	if err := r.checkLFS(gitDir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  Pushing branches and tags to %s...\n", target)
	return r.pushRefs(gitDir)
}

func (r *replicator) pushRefs(gitDir string) error {
	args := []string{"push", "--atomic", "--force"}
	if r.dryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, "replicate-target", "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	if err := r.command(gitDir, "git", args...); err != nil {
		return fmt.Errorf("push refs: %w", err)
	}
	return nil
}

func (r *replicator) syncMetadata(target string, p *plan) error {
	if err := r.call("PATCH", "repos/"+target, jsonBody(p.repository), nil); err != nil {
		return fmt.Errorf("update metadata: %w", err)
	}
	if err := r.call("PUT", "repos/"+target+"/topics", jsonBody(map[string]any{"names": p.topics}), nil); err != nil {
		return fmt.Errorf("update topics: %w", err)
	}
	for _, l := range p.labels {
		path := "repos/" + target + "/labels/" + url.PathEscape(l.Name)
		if r.dryRun {
			fmt.Fprintf(os.Stderr, "  Would upsert label %q\n", l.Name)
			continue
		}
		if err := r.call("PATCH", path, jsonBody(l), nil); err != nil {
			if !isNotFound(err) {
				return fmt.Errorf("update label %q: %w", l.Name, err)
			}
			if err := r.call("POST", "repos/"+target+"/labels", jsonBody(l), nil); err != nil {
				return fmt.Errorf("create label %q: %w", l.Name, err)
			}
		}
	}
	return nil
}

func (r *replicator) syncReleases(target string, p *plan) error {
	targetReleases, sourceReleases := p.targetReleases, p.sourceReleases
	byTag := make(map[string]release, len(targetReleases))
	for _, r := range targetReleases {
		byTag[r.TagName] = r
	}

	for _, src := range sourceReleases {
		fmt.Fprintf(os.Stderr, "  Release %s...\n", src.TagName)
		dst, exists := byTag[src.TagName]
		payload := map[string]any{
			"tag_name": src.TagName, "target_commitish": src.TargetCommitish,
			"name": src.Name, "body": src.Body, "draft": src.Draft, "prerelease": src.Prerelease,
			"make_latest": "false",
		}
		if exists {
			if err := r.call("PATCH", fmt.Sprintf("repos/%s/releases/%d", target, dst.ID), jsonBody(payload), &dst); err != nil {
				return fmt.Errorf("update release %s: %w", src.TagName, err)
			}
		} else {
			if err := r.call("POST", "repos/"+target+"/releases", jsonBody(payload), &dst); err != nil {
				return fmt.Errorf("create release %s: %w", src.TagName, err)
			}
		}
		if err := r.syncAssets(target, src, dst); err != nil {
			return err
		}
		byTag[src.TagName] = dst
	}
	// Set Latest only after every release and asset was successfully reconciled.
	if p.latest != "" {
		dst, ok := byTag[p.latest]
		if !ok {
			return fmt.Errorf("source Latest %q is absent from release snapshot; rerun", p.latest)
		}
		if err := r.call("PATCH", fmt.Sprintf("repos/%s/releases/%d", target, dst.ID), jsonBody(map[string]string{"make_latest": "true"}), nil); err != nil {
			return fmt.Errorf("set Latest: %w", err)
		}
	}
	return nil
}

func (r *replicator) syncAssets(target string, src, dst release) error {
	existing := make(map[string]asset, len(dst.Assets))
	for _, a := range dst.Assets {
		existing[a.Name] = a
	}

	for _, a := range src.Assets {
		if d, ok := existing[a.Name]; ok && sameAsset(a, d) {
			fmt.Fprintf(os.Stderr, "    Asset %s unchanged.\n", a.Name)
			continue
		}
		if r.dryRun {
			fmt.Fprintf(os.Stderr, "    Would upload/replace asset %s\n", a.Name)
			continue
		}
		fmt.Fprintf(os.Stderr, "    Replicating asset %s...\n", a.Name)

		tmp, err := os.CreateTemp("", "gh-replicate-asset-*")
		if err != nil {
			return fmt.Errorf("create temporary asset file: %w", err)
		}
		name := tmp.Name()
		defer os.Remove(name)

		if err := r.downloadAsset(a.URL, tmp); err != nil {
			tmp.Close()
			return fmt.Errorf("download asset %s: %w", a.Name, err)
		}
		if err := verifyAsset(tmp, a); err != nil {
			tmp.Close()
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			tmp.Close()
			return fmt.Errorf("rewind asset %s: %w", a.Name, err)
		}
		if d, ok := existing[a.Name]; ok {
			if err := r.call("DELETE", fmt.Sprintf("repos/%s/releases/assets/%d", target, d.ID), nil, nil); err != nil {
				tmp.Close()
				return fmt.Errorf("replace asset %s: %w", a.Name, err)
			}
		}
		if err := r.uploadAsset(target, dst.ID, a.Name, tmp); err != nil {
			tmp.Close()
			return fmt.Errorf("upload asset %s: %w", a.Name, err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("close asset %s: %w", a.Name, err)
		}
		os.Remove(name)
	}
	return nil
}

func (r *replicator) downloadAsset(path string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("asset request failed: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

func (r *replicator) uploadAsset(target string, releaseID int64, name string, src *os.File) error {
	stat, err := src.Stat()
	if err != nil {
		return err
	}

	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	path := fmt.Sprintf("https://uploads.github.com/repos/%s/releases/%d/assets?name=%s", target, releaseID, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, path, io.LimitReader(src, stat.Size()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = stat.Size()

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("asset request failed: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func sameAsset(a, b asset) bool {
	if a.Digest != "" && b.Digest != "" {
		return a.Digest == b.Digest
	}
	return false
}

func getAll[T any](r *replicator, path string) ([]T, error) {
	var all []T
	for {
		resp, err := r.client.RequestWithContext(r.ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page []T
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, page...)

		match := nextLink.FindStringSubmatch(resp.Header.Get("Link"))
		if len(match) == 0 {
			return all, nil
		}
		path = match[1]
	}
}

func jsonBody(v any) io.Reader {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(v)
	return &b
}

func (r *replicator) command(dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()

	cmd := gitCommand(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s timed out after %s", name, r.timeout)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Validate downloaded bytes before deleting an existing destination asset.
func verifyAsset(file *os.File, a asset) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != a.Size {
		return fmt.Errorf("asset %s: downloaded size differs from source", a.Name)
	}
	if strings.HasPrefix(a.Digest, "sha256:") {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			return err
		}
		if fmt.Sprintf("sha256:%x", hash.Sum(nil)) != a.Digest {
			return fmt.Errorf("asset %s: downloaded SHA-256 differs from source", a.Name)
		}
	}
	return nil
}
