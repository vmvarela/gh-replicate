package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGitEnvDropsInheritedTokens(t *testing.T) {
	t.Setenv("GH_TOKEN", "inherited")
	t.Setenv("GITHUB_TOKEN", "inherited-app")
	env := gitEnv("side-token")
	seen := 0
	for _, entry := range env {
		if entry == "GH_TOKEN=side-token" {
			seen++
			continue
		}
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			t.Errorf("inherited token survived: %q", entry)
		}
	}
	if seen != 1 {
		t.Fatalf("side token set %d times", seen)
	}
	if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("missing GIT_TERMINAL_PROMPT=0")
	}
	for _, entry := range gitEnv("") {
		if strings.HasPrefix(entry, "GH_TOKEN=") {
			t.Fatal("inherited run must not receive a token")
		}
	}
}

func TestSideResolvesRequestedAccount(t *testing.T) {
	old := tokenForAccount
	t.Cleanup(func() { tokenForAccount = old })
	tokenForAccount = func(login string) (string, error) {
		if login != "alice" {
			return "", errors.New("no oauth token found for github.com account " + login)
		}
		return "alice-token", nil
	}

	s, err := newSide("source", "alice", time.Second)
	if err != nil || s.token != "alice-token" || s.login != "alice" || s.credential != "gh auth token --user alice" {
		t.Fatalf("side = %+v, err = %v", s, err)
	}
	if _, err := newSide("target", "bob", time.Second); err == nil || !strings.Contains(err.Error(), `target account "bob"`) {
		t.Fatalf("missing account was accepted: %v", err)
	}
}

func TestIdentifyReportsAndEnforcesAccount(t *testing.T) {
	status := http.StatusOK
	r := fakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/user" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		fmt.Fprint(w, `{"login":"alice"}`)
	})

	for _, tc := range []struct {
		name  string
		login string
		want  string
		err   bool
	}{
		{"inherited", "", "alice", false},
		{"matching", "Alice", "alice", false},
		{"different", "bob", "", true},
	} {
		s := r.source
		s.login = tc.login
		login, err := r.identify("source", s)
		if login != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: login=%q err=%v", tc.name, login, err)
		}
	}

	// An unreadable identity only aborts when an account was requested.
	status = http.StatusInternalServerError
	inherited := r.source
	inherited.login = ""
	if login, err := r.identify("source", inherited); login != "" || err != nil {
		t.Fatalf("inherited identity must not fail: %q %v", login, err)
	}
	requested := r.source
	requested.login = "alice"
	if _, err := r.identify("source", requested); err == nil || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("requested identity ignored the failure: %v", err)
	}
}

func accountServer(t *testing.T, logins map[string]string, paths *[]string) string {
	t.Helper()
	return fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
		*paths = append(*paths, req.URL.Path)
		fields := strings.Fields(req.Header.Get("Authorization"))
		switch {
		case req.URL.Path == "/user":
			login := ""
			if len(fields) == 2 {
				login = logins[fields[1]]
			}
			fmt.Fprintf(w, `{"login":%q}`, login)
		case strings.HasSuffix(req.URL.Path, "/topics"):
			fmt.Fprint(w, `{"names":[]}`)
		case strings.HasSuffix(req.URL.Path, "/labels"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(req.URL.Path, "/releases/latest"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(req.URL.Path, "/releases"):
			fmt.Fprint(w, `[]`)
		case strings.HasPrefix(req.URL.Path, "/repos/"):
			fmt.Fprint(w, `{"description":"","homepage":""}`)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})
}

func TestPreflightReportsOneAccountPerSide(t *testing.T) {
	var paths []string
	server := accountServer(t, map[string]string{"token-source": "alice", "token-target": "bob"}, &paths)
	src := fakeSide(t, server, "token-source")
	src.login, src.credential = "alice", "gh auth token --user alice"
	dst := fakeSide(t, server, "token-target")
	dst.login, dst.credential = "bob", "gh auth token --user bob"
	r := &replicator{source: src, target: dst, ctx: context.Background(), timeout: time.Minute}

	out := captureStderr(t, func() {
		if _, err := r.preflight("alice/project", "bob/project-replica"); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "Source identity: alice (gh auth token --user alice)") ||
		!strings.Contains(out, "Target identity: bob (gh auth token --user bob)") {
		t.Fatalf("identities not reported: %q", out)
	}
}

func TestPreflightAbortsBeforeWritesOnWrongAccount(t *testing.T) {
	var paths []string
	server := accountServer(t, map[string]string{"token-source": "mallory"}, &paths)
	src := fakeSide(t, server, "token-source")
	src.login = "alice"
	r := &replicator{source: src, target: src, ctx: context.Background(), timeout: time.Minute}

	_, err := r.preflight("alice/project", "bob/project-replica")
	if err == nil || !strings.Contains(err.Error(), "mallory") || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("mismatch was not reported: %v", err)
	}
	if !reflect.DeepEqual(paths, []string{"/user"}) {
		t.Fatalf("requests after the mismatch: %v", paths)
	}
}

func TestPreflightDisablesDestinationActions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		read    int
		body    string
		put     int
		want    string
		wantPut bool
	}{
		{"enabled", http.StatusOK, `{"enabled":true}`, http.StatusOK, "Disabled GitHub Actions", true},
		{"disabled", http.StatusOK, `{"enabled":false}`, http.StatusOK, "", false},
		{"unreadable", http.StatusForbidden, `{"message":"Resource not accessible"}`, http.StatusOK, "could not check", false},
		{"rejected", http.StatusOK, `{"enabled":true}`, http.StatusForbidden, "could not be disabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var puts int
			server := fakeServer(t, func(w http.ResponseWriter, req *http.Request) {
				switch {
				case req.URL.Path == "/user":
					fmt.Fprint(w, `{"login":"alice"}`)
				case strings.HasSuffix(req.URL.Path, "/actions/permissions"):
					if req.Method == http.MethodPut {
						puts++
						if tc.put != http.StatusOK {
							w.WriteHeader(tc.put)
							fmt.Fprint(w, `{"message":"Resource not accessible"}`)
							return
						}
						var body struct {
							Enabled bool `json:"enabled"`
						}
						if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body.Enabled {
							t.Errorf("PUT kept Actions enabled")
						}
						fmt.Fprint(w, `{}`)
						return
					}
					w.WriteHeader(tc.read)
					fmt.Fprint(w, tc.body)
				case strings.HasSuffix(req.URL.Path, "/topics"):
					fmt.Fprint(w, `{"names":[]}`)
				case strings.HasSuffix(req.URL.Path, "/labels"):
					fmt.Fprint(w, `[]`)
				case strings.HasSuffix(req.URL.Path, "/releases/latest"):
					w.WriteHeader(http.StatusNotFound)
				case strings.HasSuffix(req.URL.Path, "/releases"):
					fmt.Fprint(w, `[]`)
				default:
					fmt.Fprint(w, `{"description":"","homepage":""}`)
				}
			})
			side := fakeSide(t, server, "token")
			r := &replicator{source: side, target: side, ctx: context.Background(), timeout: time.Minute}

			out := captureStderr(t, func() {
				if _, err := r.preflight("alice/project", "bob/project-replica"); err != nil {
					t.Fatal(err)
				}
			})
			if got := puts > 0; got != tc.wantPut {
				t.Fatalf("sent update: %v, want %v (output %q)", got, tc.wantPut, out)
			}
			if tc.want == "" {
				if strings.Contains(out, "Warning") || strings.Contains(out, "Disabled GitHub Actions") {
					t.Fatalf("unexpected output: %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("missing %q in %q", tc.want, out)
			}
		})
	}
}
