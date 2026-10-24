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

// side is one endpoint's credentials: an already-authenticated gh account or
// the inherited one. login is set only when an account was requested.
type side struct {
	client     *api.RESTClient
	httpClient *http.Client
	token      string
	login      string
	credential string
}

type replicator struct {
	source  side
	target  side
	ctx     context.Context
	timeout time.Duration
	dryRun  bool
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

// gitRemote is the URL the bare clone and its destination remote use.
var gitRemote = func(ownerRepo string) string { return "https://github.com/" + ownerRepo + ".git" }

func run(args []string) error {
	flags := flag.NewFlagSet("gh replicate", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "inspect replication without changing the destination")
	timeout := flags.Duration("timeout", 30*time.Minute, "timeout per Git command or HTTP request")
	sourceAccount := flags.String("source-account", "", "authenticated gh account login used to read the source")
	targetAccount := flags.String("target-account", "", "authenticated gh account login used to write the destination")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: gh replicate [--dry-run] [--timeout 30m] [--source-account LOGIN] [--target-account LOGIN] SOURCE TARGET")
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
	src, err := newSide("source", *sourceAccount, *timeout)
	if err != nil {
		return err
	}
	dst, err := newSide("target", *targetAccount, *timeout)
	if err != nil {
		return err
	}
	r := &replicator{source: src, target: dst, ctx: ctx, timeout: *timeout, dryRun: *dryRun}
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

// newSide resolves one side's credentials without touching the active account,
// the global Git configuration or the credential store.
var newSide = newSideFromAccounts

func newSideFromAccounts(role, account string, timeout time.Duration) (side, error) {
	s := side{login: account}
	if account != "" {
		token, err := tokenForAccount(account)
		if err != nil {
			return side{}, fmt.Errorf("%s account %q: %w", role, account, err)
		}
		s.token = token
		s.credential = "gh auth token --user " + account
	} else {
		s.token, s.credential = auth.TokenForHost("github.com")
	}
	opts := api.ClientOptions{Host: "github.com", AuthToken: s.token, Timeout: timeout, Transport: retryTransport{base: http.DefaultTransport}}
	client, err := api.NewRESTClient(opts)
	if err != nil {
		return side{}, err
	}
	httpClient, err := api.NewHTTPClient(opts)
	if err != nil {
		return side{}, err
	}
	s.client, s.httpClient = client, httpClient
	return s, nil
}

// call is the only JSON write path; dry runs never send mutating HTTP requests.
func (r *replicator) call(s side, method, path string, body io.Reader, out any) error {
	if r.dryRun && method != http.MethodGet {
		fmt.Fprintf(os.Stderr, "  Would %s %s\n", method, path)
		return nil
	}
	return s.client.DoWithContext(r.ctx, method, path, body, out)
}

// identify proves which account a side actually acts as. Without a requested
// account the login is reported but not enforced, so the inherited path keeps
// working with tokens that cannot read /user.
func (r *replicator) identify(role string, s side) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := r.call(s, "GET", "user", nil, &user); err != nil {
		if s.login == "" {
			fmt.Fprintf(os.Stderr, "  %s identity unavailable: %v\n", role, err)
			return "", nil
		}
		return "", fmt.Errorf("%s identity for account %q: %w", role, s.login, err)
	}
	if s.login != "" && !strings.EqualFold(user.Login, s.login) {
		return "", fmt.Errorf("%s token authenticates as %q, not the requested account %q; nothing was changed", role, user.Login, s.login)
	}
	return user.Login, nil
}

// verifyIdentities runs before the clone and before any write.
func (r *replicator) verifyIdentities() error {
	src, err := r.identify("source", r.source)
	if err != nil {
		return err
	}
	dst, err := r.identify("target", r.target)
	if err != nil {
		return err
	}
	if src != "" && src == dst {
		fmt.Fprintf(os.Stderr, "Identity: %s (%s) for both sides\n", src, r.source.credential)
		return nil
	}
	if src != "" {
		fmt.Fprintf(os.Stderr, "Source identity: %s (%s)\n", src, r.source.credential)
	}
	if dst != "" {
		fmt.Fprintf(os.Stderr, "Target identity: %s (%s)\n", dst, r.target.credential)
	}
	return nil
}

// warnIfActionsRun reports that a push can start destination workflows. The
// check is advisory: an unreadable setting is reported, never fatal.
func (r *replicator) warnIfActionsRun(target string) {
	var permissions struct {
		Enabled bool `json:"enabled"`
	}
	if err := r.call(r.target, "GET", "repos/"+target+"/actions/permissions", nil, &permissions); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not check whether GitHub Actions are enabled in %s: %v\n", target, err)
		return
	}
	if permissions.Enabled {
		fmt.Fprintf(os.Stderr, "Warning: GitHub Actions are enabled in %s, so this replication can start its workflows. For a passive mirror, disable them once: Settings > Actions > General > Actions permissions > Disable actions > Save.\n", target)
	}
}

func (r *replicator) preflight(source, target string) (*plan, error) {
	if err := r.verifyIdentities(); err != nil {
		return nil, err
	}
	r.warnIfActionsRun(target)
	p := &plan{}
	if err := r.call(r.source, "GET", "repos/"+source, nil, &p.repository); err != nil {
		return nil, err
	}
	var dst repository
	if err := r.call(r.target, "GET", "repos/"+target, nil, &dst); err != nil {
		return nil, err
	}
	var srcTopics, dstTopics struct {
		Names []string `json:"names"`
	}
	if err := r.call(r.source, "GET", "repos/"+source+"/topics", nil, &srcTopics); err != nil {
		return nil, err
	}
	if err := r.call(r.target, "GET", "repos/"+target+"/topics", nil, &dstTopics); err != nil {
		return nil, err
	}
	var err error
	p.topics, err = unionTopics(srcTopics.Names, dstTopics.Names)
	if err != nil {
		return nil, err
	}
	p.labels, err = getAll[label](r, r.source, "repos/"+source+"/labels?per_page=100")
	if err != nil {
		return nil, err
	}
	p.sourceReleases, err = getAll[release](r, r.source, "repos/"+source+"/releases?per_page=100")
	if err != nil {
		return nil, err
	}
	p.targetReleases, err = getAll[release](r, r.target, "repos/"+target+"/releases?per_page=100")
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
	if err := r.call(r.source, "GET", "repos/"+source+"/releases/latest", nil, &latest); err != nil {
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
	if err := r.command("", r.source, "git", "clone", "--bare", gitRemote(source), gitDir); err != nil {
		return fmt.Errorf("clone source: %w", err)
	}
	if err := r.command(gitDir, r.source, "git", "remote", "add", "replicate-target", gitRemote(target)); err != nil {
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
	if err := r.command(gitDir, r.target, "git", args...); err != nil {
		return fmt.Errorf("push refs: %w", err)
	}
	return nil
}

func (r *replicator) syncMetadata(target string, p *plan) error {
	if err := r.call(r.target, "PATCH", "repos/"+target, jsonBody(p.repository), nil); err != nil {
		return fmt.Errorf("update metadata: %w", err)
	}
	if err := r.call(r.target, "PUT", "repos/"+target+"/topics", jsonBody(map[string]any{"names": p.topics}), nil); err != nil {
		return fmt.Errorf("update topics: %w", err)
	}
	for _, l := range p.labels {
		path := "repos/" + target + "/labels/" + url.PathEscape(l.Name)
		if r.dryRun {
			fmt.Fprintf(os.Stderr, "  Would upsert label %q\n", l.Name)
			continue
		}
		if err := r.call(r.target, "PATCH", path, jsonBody(l), nil); err != nil {
			if !isNotFound(err) {
				return fmt.Errorf("update label %q: %w", l.Name, err)
			}
			if err := r.call(r.target, "POST", "repos/"+target+"/labels", jsonBody(l), nil); err != nil {
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
			if err := r.call(r.target, "PATCH", fmt.Sprintf("repos/%s/releases/%d", target, dst.ID), jsonBody(payload), &dst); err != nil {
				return fmt.Errorf("update release %s: %w", src.TagName, err)
			}
		} else {
			if err := r.call(r.target, "POST", "repos/"+target+"/releases", jsonBody(payload), &dst); err != nil {
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
		if r.dryRun {
			fmt.Fprintf(os.Stderr, "  Would set Latest to release %s after reconciliation\n", p.latest)
		} else if err := r.call(r.target, "PATCH", fmt.Sprintf("repos/%s/releases/%d", target, dst.ID), jsonBody(map[string]string{"make_latest": "true"}), nil); err != nil {
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
		d, exists := existing[a.Name]
		if exists && sameAsset(a, d) {
			fmt.Fprintf(os.Stderr, "    Asset %s unchanged.\n", a.Name)
			continue
		}
		if r.dryRun {
			fmt.Fprintf(os.Stderr, "    Would upload/replace asset %s\n", a.Name)
			continue
		}
		fmt.Fprintf(os.Stderr, "    Replicating asset %s...\n", a.Name)
		if err := func() error {
			tmp, err := os.CreateTemp("", "gh-replicate-asset-*")
			if err != nil {
				return fmt.Errorf("create temporary asset file: %w", err)
			}
			defer os.Remove(tmp.Name())
			defer tmp.Close()

			if err := r.downloadAsset(r.source, a.URL, tmp); err != nil {
				return fmt.Errorf("download asset %s: %w", a.Name, err)
			}
			digest, err := verifyAsset(tmp, a)
			if err != nil {
				return err
			}
			if exists {
				equal, err := r.sameContent(a, d, digest)
				if err != nil {
					return fmt.Errorf("compare asset %s: %w", a.Name, err)
				}
				if equal {
					fmt.Fprintf(os.Stderr, "    Asset %s unchanged.\n", a.Name)
					return nil
				}
			}
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewind asset %s: %w", a.Name, err)
			}
			if exists {
				if err := r.call(r.target, "DELETE", fmt.Sprintf("repos/%s/releases/assets/%d", target, d.ID), nil, nil); err != nil {
					return fmt.Errorf("replace asset %s: %w", a.Name, err)
				}
			}
			if err := r.uploadAsset(target, dst.ID, a.Name, tmp, digest); err != nil {
				return fmt.Errorf("upload asset %s: %w", a.Name, err)
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	return nil
}

// sameContent hashes the destination only when its digest cannot decide.
func (r *replicator) sameContent(a, d asset, digest string) (bool, error) {
	if strings.HasPrefix(d.Digest, "sha256:") {
		return d.Digest == digest, nil
	}
	if d.Size != a.Size {
		return false, nil
	}
	if d.URL == "" {
		return false, errors.New("destination asset URL is missing")
	}
	hash := sha256.New()
	if err := r.downloadAsset(r.target, d.URL, hash); err != nil {
		return false, err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)) == digest, nil
}

func (r *replicator) downloadAsset(s side, path string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := s.httpClient.Do(req)
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

func (r *replicator) uploadAsset(target string, releaseID int64, name string, src *os.File, digest string) error {
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

	resp, err := r.target.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read asset response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(body) > 4096 {
			body = body[:4096]
		}
		return fmt.Errorf("asset request failed: HTTP %d: %q", resp.StatusCode, body)
	}
	var uploaded asset
	if err := json.Unmarshal(body, &uploaded); err != nil {
		return fmt.Errorf("decode asset response: %w (body %q)", err, body)
	}
	if uploaded.Size != stat.Size() {
		return fmt.Errorf("asset %s: uploaded size %d, expected %d", name, uploaded.Size, stat.Size())
	}
	if strings.HasPrefix(uploaded.Digest, "sha256:") && uploaded.Digest != digest {
		return fmt.Errorf("asset %s: uploaded SHA-256 differs from source", name)
	}
	return nil
}

func sameAsset(a, b asset) bool {
	if a.Digest != "" && b.Digest != "" {
		return a.Digest == b.Digest
	}
	return false
}

func getAll[T any](r *replicator, s side, path string) ([]T, error) {
	var all []T
	for {
		resp, err := s.client.RequestWithContext(r.ctx, http.MethodGet, path, nil)
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

func (r *replicator) command(dir string, s side, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()

	cmd := gitCommand(ctx, s.token, name, args...)
	cmd.Dir = dir
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
func verifyAsset(file *os.File, a asset) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() != a.Size {
		return "", fmt.Errorf("asset %s: downloaded size differs from source", a.Name)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	digest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if strings.HasPrefix(a.Digest, "sha256:") && digest != a.Digest {
		return "", fmt.Errorf("asset %s: downloaded SHA-256 differs from source", a.Name)
	}
	return digest, nil
}
