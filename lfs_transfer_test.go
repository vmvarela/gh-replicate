package main

import "testing"

func TestLFSParseVersion(t *testing.T) {
	cases := map[string][3]int{
		"git-lfs/3.8.1 (GitHub; darwin arm64; go 1.27.3; git a662a6db)": {3, 8, 1},
		"git-lfs/3.8.1\n": {3, 8, 1},
		"git-lfs/2.11.0 (GitHub; linux amd64; go 1.15)": {2, 11, 0},
	}
	for in, want := range cases {
		got, err := parseLFSVersion(in)
		if err != nil || got != want {
			t.Fatalf("parseLFSVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"",
		"lfs/3.8.1",
		"git-lfs/3.8",
		"git-lfs/3.8.x",
		"git-lfs/version",
		"3.8.1",
	} {
		if _, err := parseLFSVersion(bad); err == nil {
			t.Fatalf("parseLFSVersion(%q) accepted", bad)
		}
	}
}
