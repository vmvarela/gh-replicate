package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func assetDigest(s string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(s)))
}

func TestValidRepo(t *testing.T) {
	for input, want := range map[string]bool{"owner/repo": true, "owner/": false, "/repo": false, "repo": false, "a/b/c": false, "a/b?x": false, "a/..": false} {
		if got := validRepo(input); got != want {
			t.Errorf("validRepo(%q) = %v, want %v", input, got, want)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAssetHTTP(t *testing.T) {
	r := &replicator{ctx: context.Background()}
	for _, status := range []int{200, 401, 404, 500} {
		r.httpClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("missing binary accept header")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("binary")), Header: make(http.Header)}, nil
		})}
		var dst bytes.Buffer
		err := r.downloadAsset("https://api.github.com/repos/a/b/releases/assets/1", &dst)
		if (err == nil) != (status == 200) {
			t.Errorf("status %d: %v", status, err)
		}
		if status != 200 && dst.Len() != 0 {
			t.Error("downloaded error body as asset")
		}
	}
}
func TestUploadAsset(t *testing.T) {
	r := &replicator{ctx: context.Background()}
	f, err := os.CreateTemp(t.TempDir(), "asset")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString("binary"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{201, 422, 500} {
		if _, err = f.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		r.httpClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "uploads.github.com" || r.URL.Path != "/repos/a/b/releases/123/assets" || r.URL.Query().Get("name") != "file +.zip" {
				t.Errorf("wrong upload URL: %s", r.URL)
			}
			if r.Method != "POST" || r.ContentLength != 6 {
				t.Error("wrong upload request")
			}
			if _, err := io.ReadAll(r.Body); err != nil {
				t.Error(err)
			}
			if err := r.Body.Close(); err != nil {
				t.Error(err)
			}
			body := "{}"
			if status == 201 {
				body = fmt.Sprintf(`{"name":"file +.zip","size":6,"digest":%q}`, assetDigest("binary"))
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		err = r.uploadAsset("a/b", 123, "file +.zip", f, assetDigest("binary"))
		if (err == nil) != (status == 201) {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

func TestUploadAssetValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"missing digest", `{"size":6}`, false},
		{"wrong size", `{"size":5,"digest":"` + assetDigest("binary") + `"}`, true},
		{"wrong digest", `{"size":6,"digest":"sha256:bad"}`, true},
		{"malformed JSON", `{`, true},
		{"empty body", ``, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "asset")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("binary"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			r := &replicator{ctx: context.Background()}
			r.httpClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			err = r.uploadAsset("a/b", 123, "file +.zip", f, assetDigest("binary"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestUploadErrorBodyBounded(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "asset")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("binary"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 5000)
	r := &replicator{ctx: context.Background()}
	r.httpClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	err = r.uploadAsset("a/b", 123, "file +.zip", f, assetDigest("binary"))
	if err == nil || !strings.Contains(err.Error(), strings.Repeat("x", 4096)) || strings.Contains(err.Error(), strings.Repeat("x", 4097)) {
		t.Fatalf("error body was not bounded: %v", err)
	}
}
func TestSameAsset(t *testing.T) {
	if sameAsset(asset{Size: 4}, asset{Size: 4}) {
		t.Fatal("size alone cannot prove identical contents")
	}
	if sameAsset(asset{Digest: "sha256:a"}, asset{Digest: "sha256:b"}) {
		t.Fatal("different digests")
	}
	if !sameAsset(asset{Digest: "sha256:a"}, asset{Digest: "sha256:a"}) {
		t.Fatal("matching digests")
	}
}
