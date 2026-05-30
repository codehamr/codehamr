package main

import "testing"

// TestIsLocalBuild pins the contract: `go run` ("dev") and dirty-tree builds
// are local and skip self-update — else an older release overwrites unreleased
// work. Clean semver tags still self-update.
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
