package main

import (
	"os"
	"testing"
)

// TestIsLocalBuild pins the contract: `go run` ("dev") and dirty-tree builds
// are local and skip self-update; else an older release overwrites unreleased
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

// TestReexecGuardOverridesPreexistingValue pins the loop-guard env semantics
// maybeSelfUpdate relies on: the re-exec'd child must see
// CODEHAMR_NO_UPDATE_CHECK=="1" even when a user already exported a different
// value. os.Setenv (the fix) overwrites in place; the old append(os.Environ(),…)
// left the stale value first, which Unix execve resolves first, defeating the
// guard. update.Check short-circuits only on exactly "1".
func TestReexecGuardOverridesPreexistingValue(t *testing.T) {
	t.Setenv("CODEHAMR_NO_UPDATE_CHECK", "0") // user set it wrong; restored after test
	os.Setenv("CODEHAMR_NO_UPDATE_CHECK", "1")
	if got := os.Getenv("CODEHAMR_NO_UPDATE_CHECK"); got != "1" {
		t.Fatalf("guard env resolves to %q, want \"1\" - append() would have left \"0\" first", got)
	}
}

// TestAutoUpdateDisabledByDefault pins the opt-in contract: without
// CODEHAMR_AUTO_UPDATE set, maybeSelfUpdate's gate must read non-"1" so the
// pre-launch freshness check never runs. Auto-update is off by default; only
// CODEHAMR_AUTO_UPDATE=1 turns it on (mirrors the CODEHAMR_NO_UPDATE_CHECK
// opt-out semantics).
func TestAutoUpdateDisabledByDefault(t *testing.T) {
	t.Setenv("CODEHAMR_AUTO_UPDATE", "")
	if os.Getenv("CODEHAMR_AUTO_UPDATE") == "1" {
		t.Fatal("auto-update gate must be off when CODEHAMR_AUTO_UPDATE is unset")
	}
	t.Setenv("CODEHAMR_AUTO_UPDATE", "0")
	if os.Getenv("CODEHAMR_AUTO_UPDATE") == "1" {
		t.Fatal("auto-update gate must be off when CODEHAMR_AUTO_UPDATE != \"1\"")
	}
	t.Setenv("CODEHAMR_AUTO_UPDATE", "1")
	if os.Getenv("CODEHAMR_AUTO_UPDATE") != "1" {
		t.Fatal("auto-update gate must be on when CODEHAMR_AUTO_UPDATE=1")
	}
}
