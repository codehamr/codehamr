package main

import "testing"

// TestIsLocalBuild pins down the contract: `go run` (version=="dev") and
// dirty-tree installs (`v…-dirty`) are treated as local and must skip
// self-update, otherwise an older release silently overwrites the
// freshly-compiled binary carrying unreleased work. Clean semver tags
// (goreleaser output) continue to self-update.
func TestIsLocalBuild(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"dev", true},
		{"v1.2.3-dirty", true},
		{"v0.1.0-5-g1a2b3c4-dirty", true},
		{"v1.2.3", false},
		{"v0.1.0-5-g1a2b3c4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLocalBuild(c.in); got != c.want {
			t.Errorf("isLocalBuild(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
