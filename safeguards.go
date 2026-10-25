package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func isNotFound(err error) bool {
	var e *api.HTTPError
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

func unionTopics(source, target []string) ([]string, error) {
	names := []string{}
	seen := map[string]bool{}
	for _, name := range append(append([]string{}, source...), target...) {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	if len(names) > 20 {
		return nil, fmt.Errorf("combined topics exceed GitHub's limit of 20 (%d); no topics will be discarded", len(names))
	}
	return names, nil
}

func uniqueTags(releases []release) error {
	seen := map[string]bool{}
	for _, r := range releases {
		if r.TagName == "" || seen[r.TagName] {
			return fmt.Errorf("ambiguous release tag %q; assign a unique tag to every draft/release before replicating", r.TagName)
		}
		seen[r.TagName] = true
	}
	return nil
}

// Configuration applies only to this child process. Never write ~/.gitconfig.
func gitCommand(ctx context.Context, token, name string, args ...string) *exec.Cmd {
	if name == "git" {
		args = append([]string{"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!gh auth git-credential", "-c", "http.https://github.com/.extraheader="}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = gitEnv(token)
	return cmd
}

// A side's token must replace the inherited one: duplicate entries are
// resolved inconsistently by credential helpers.
func gitEnv(token string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			continue
		}
		env = append(env, entry)
	}
	if token != "" {
		env = append(env, "GH_TOKEN="+token)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// tokenForAccount reads a token the user already stored for that account; it
// never switches the active account or writes the credential store.
var tokenForAccount = func(login string) (string, error) {
	out, err := exec.Command("gh", "auth", "token", "--hostname", "github.com", "--user", login).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
			return "", errors.New(strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Detect LFS pointers reachable from the branches and tags being pushed,
// including historical versions, but excluding obsolete cached objects.
func (r *replicator) checkLFS(dir string) error {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	reachable := r.gitCommand(ctx, "", "git", "rev-list", "--objects", "--no-object-names", "--branches", "--tags")
	check := r.gitCommand(ctx, "", "git", "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	read := r.gitCommand(ctx, "", "git", "cat-file", "--batch")
	for _, cmd := range []*exec.Cmd{reachable, check, read} {
		cmd.Dir = dir
	}
	objectIDs, err := reachable.StdoutPipe()
	if err != nil {
		return err
	}
	defer objectIDs.Close()
	check.Stdin = objectIDs
	metadata, err := check.StdoutPipe()
	if err != nil {
		return err
	}
	defer metadata.Close()
	requests, err := read.StdinPipe()
	if err != nil {
		return err
	}
	defer requests.Close()
	contents, err := read.StdoutPipe()
	if err != nil {
		return err
	}
	defer contents.Close()
	// Cancel and reap every started child on early detection, errors or timeout.
	var started []*exec.Cmd
	defer func() {
		cancel()
		for _, cmd := range started {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}
	}()
	for _, cmd := range []*exec.Cmd{read, check, reachable} {
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start LFS scan: %w", err)
		}
		started = append(started, cmd)
	}
	reader := bufio.NewReader(contents)
	scanner := bufio.NewScanner(metadata)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return errors.New("invalid git cat-file output")
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return errors.New("invalid Git object size")
		}
		if fields[1] != "blob" || size == 0 || size > 1024 {
			continue
		}
		if _, err := fmt.Fprintln(requests, fields[0]); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		header := strings.Fields(line)
		if len(header) != 3 || header[0] != fields[0] || header[1] != "blob" || header[2] != fields[2] {
			return errors.New("invalid git cat-file object header")
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(reader, data); err != nil {
			return err
		}
		if bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1\n")) {
			return errors.New("Git LFS pointers detected; LFS objects are not supported, so replication stopped before destination changes")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := requests.Close(); err != nil {
		return err
	}
	for _, cmd := range []*exec.Cmd{reachable, check, read} {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("scan Git objects for LFS: %w", err)
		}
	}
	return nil
}

// Only reads are replayed. Retrying a POST after an ambiguous failure could
// create duplicate releases/assets. The next invocation reconciles writes.
type retryTransport struct{ base http.RoundTripper }

func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	retry := resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 || (resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if !retry {
		return 0, false
	}
	delay := time.Second << attempt
	if value := resp.Header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			delay = time.Duration(seconds) * time.Second
		} else if date, err := http.ParseTime(value); err == nil {
			delay = time.Until(date)
		} else {
			return 0, false
		}
	} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		if err != nil {
			return 0, false
		}
		delay = time.Until(time.Unix(reset, 0))
	}
	if delay < 0 {
		delay = 0
	}
	if delay > 30*time.Second {
		return 0, false
	}
	return delay, true
}

func (t retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil || req.Method != http.MethodGet || attempt >= 2 {
			return resp, err
		}
		delay, retry := retryDelay(resp, attempt)
		if !retry {
			return resp, nil
		}
		_ = resp.Body.Close()
		fmt.Fprintf(os.Stderr, "  Read returned HTTP %d; retrying in %s...\n", resp.StatusCode, delay)
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}
