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
		if !seen[name] { names = append(names, name); seen[name] = true }
	}
	if len(names) > 20 { return nil, fmt.Errorf("combined topics exceed GitHub's limit of 20 (%d); no topics will be discarded", len(names)) }
	return names, nil
}

func uniqueTags(releases []release) error {
	seen := map[string]bool{}
	for _, r := range releases {
		if r.TagName == "" || seen[r.TagName] { return fmt.Errorf("ambiguous release tag %q; assign a unique tag to every draft/release before replicating", r.TagName) }
		seen[r.TagName] = true
	}
	return nil
}

// Configuration applies only to this child process. Never write ~/.gitconfig.
func gitCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name == "git" {
		args = append([]string{"-c", "credential.helper=", "-c", "credential.https://github.com.helper=!gh auth git-credential", "-c", "http.https://github.com/.extraheader="}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// Detect LFS pointers in all objects of the bare clone, including historical
// versions. Copying only Git refs cannot make these large objects available.
func (r *replicator) checkLFS(dir string) error {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	check := gitCommand(ctx, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	check.Dir = dir
	objects, err := check.Output()
	if err != nil { return fmt.Errorf("inspect Git objects for LFS: %w", err) }
	var ids bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(objects))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 { return errors.New("invalid git cat-file output") }
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil { return err }
		if fields[1] == "blob" && size > 0 && size <= 1024 { fmt.Fprintln(&ids, fields[0]) }
	}
	if err := scanner.Err(); err != nil { return err }
	if ids.Len() == 0 { return nil }
	read := gitCommand(ctx, "git", "cat-file", "--batch")
	read.Dir = dir
	read.Stdin = &ids
	stdout, err := read.StdoutPipe()
	if err != nil { return err }
	if err := read.Start(); err != nil { return err }
	// On a detected pointer or malformed output, stop and reap the child.
	defer func() { if read.ProcessState == nil { _ = read.Process.Kill(); _ = read.Wait() } }()
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF { break }
		if err != nil { return err }
		fields := strings.Fields(line)
		if len(fields) != 3 { return errors.New("invalid git cat-file object header") }
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size > 1024 { return errors.New("invalid Git object size") }
		data := make([]byte, size+1)
		if _, err := io.ReadFull(reader, data); err != nil { return err }
		if bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1\n")) {
			return errors.New("Git LFS pointers detected; LFS objects are not supported, so replication stopped before destination changes")
		}
	}
	return read.Wait()
}

// Only reads are replayed. Retrying a POST after an ambiguous failure could
// create duplicate releases/assets. The next invocation reconciles writes.
type retryTransport struct { base http.RoundTripper }

func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	retry := resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 || (resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if !retry { return 0, false }
	delay := time.Second << attempt
	if value := resp.Header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil { delay = time.Duration(seconds)*time.Second } else if date, err := http.ParseTime(value); err == nil { delay = time.Until(date) } else { return 0, false }
	} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		if err != nil { return 0, false }
		delay = time.Until(time.Unix(reset, 0))
	}
	if delay < 0 { delay = 0 }
	if delay > 30*time.Second { return 0, false }
	return delay, true
}

func (t retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil || req.Method != http.MethodGet || attempt >= 2 { return resp, err }
		delay, retry := retryDelay(resp, attempt)
		if !retry { return resp, nil }
		_ = resp.Body.Close()
		fmt.Fprintf(os.Stderr, "  Read returned HTTP %d; retrying in %s...\n", resp.StatusCode, delay)
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done(): timer.Stop(); return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}
