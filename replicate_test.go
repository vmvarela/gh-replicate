package main

import "testing"

func TestValidRepo(t *testing.T) {
	tests := map[string]bool{"owner/repo": true, "owner/": false, "/repo": false, "repo": false, "a/b/c": false}
	for input, want := range tests {
		if got := validRepo(input); got != want { t.Fatalf("validRepo(%q) = %v, want %v", input, got, want) }
	}
}
