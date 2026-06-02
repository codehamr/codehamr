// Command codehamr is the lightweight, fast coding agent for the terminal.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tui"
	"github.com/codehamr/codehamr/internal/update"
)

// updateBudget caps the pre-launch auto-update (checksum fetch + download +
// rename): enough for a ~10MB binary on a slow link, short enough that an
// offline user isn't stalled before the TUI appears.
const updateBudget = 20 * time.Second

// version is injected via -ldflags at build time; "dev" when running `go run`.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Println("codehamr", version)
			return
		case "-h", "--help", "help":
			printHelp()
			return
		}
	}

	// Wipe last session's superseded binary. Apply renames the running exe
	// to <path>.old before promoting the new one; on Windows that old file
	// stays locked until the prior process exits, so deleting it at launch
	// (not at Apply time) always wins.
	if exe, err := os.Executable(); err == nil {
		update.CleanupOld(exe)
	}

	// Pre-launch auto-update; all failures are non-fatal and fall through to
	// the old binary.
	maybeSelfUpdate()

	cwd := mustCwd()
	// created is ignored: any first-run notice printed here is wiped milliseconds
	// later by the unconditional screen+scrollback clear below, before the TUI
	// draws, so there's nothing to announce.
	cfg, _, err := config.Bootstrap(cwd)
	if err != nil {
		log.Fatalf("codehamr: %v", err)
	}
	applyEnvOverrides(cfg)

	// Opt-in debug log (`logging: true`): truncates .codehamr/log.txt and
	// records every chat exchange. See tui.OpenDebugLog / dbgWrite.
	if cfg.Logging {
		tui.OpenDebugLog(cfg.Dir)
		defer tui.CloseDebugLog()
	}

	p := cfg.ActiveProfile()
	client := llm.New(cfg.ActiveURL(), p.LLM, p.Key)

	abs, _ := filepath.Abs(cwd)
	m := tui.New(cfg, client, abs, version)

	// Hard clear before the TUI takes over: \x1b[2J viewport, \x1b[3J
	// scrollback, \x1b[H cursor home, a clean canvas free of prior shell
	// history. Inline-mode safe: the session's own scrollback still
	// accumulates via tea.Println.
	os.Stdout.WriteString("\x1b[2J\x1b[3J\x1b[H")

	// Inline mode (no AltScreen, no mouse capture): only the prompt + status
	// bar render live at the bottom; everything else goes to native
	// scrollback via tea.Println, leaving scrolling/selection/copy to the
	// terminal.
	//
	// WithReportFocus types raw focus-in/out sequences (\x1b[I / \x1b[O) as
	// tea.FocusMsg / tea.BlurMsg so Update can swallow them; otherwise
	// xterm.js hosts (VS Code) leak those bytes as runes into the textarea
	// on every window switch, inflating prompt height with invisible chars.
	if _, err := tea.NewProgram(m, tea.WithReportFocus()).Run(); err != nil {
		log.Fatalf("codehamr: %v", err)
	}
}

func printHelp() {
	fmt.Println(strings.TrimSpace(`
codehamr, a lightweight, fast coding agent for the terminal.

Usage:
  codehamr             start interactive TUI
  codehamr --version   print version

Slash commands (inside TUI):`))
	tui.PrintHelp(os.Stdout)
	fmt.Println(strings.TrimSpace(`
Keys (inside TUI):
  ctrl+l   clear the screen (keeps conversation)
  ctrl+c   cancel running op · press again to quit
  ctrl+d   quit (on empty input)

Config:
  .codehamr/config.yaml   per-project settings

Env:
  CODEHAMR_URL         override the active profile's url at runtime`))
}

// isLocalBuild reports whether the binary came from a working tree rather
// than an official release. `go run` leaves version "dev"; `make install` on
// a dirty tree adds a "-dirty" suffix. Goreleaser tags read as non-local and
// still self-update.
func isLocalBuild(version string) bool {
	return version == "dev" || strings.HasSuffix(version, "-dirty")
}

// maybeSelfUpdate runs the pre-launch auto-update. No-op for local builds,
// an already-current hash, an unsupported platform (see update.assetName),
// or any network/filesystem refusal. On success it swaps the binary and
// re-execs via reExec (which only returns on failure). Any failure past
// "update available" prints one stderr line and proceeds with the old binary.
func maybeSelfUpdate() {
	// Skip local builds: hashing a `go run` temp binary against the
	// published checksum would otherwise swap in the last release and hide
	// unreleased work behind an "update applied" banner.
	if isLocalBuild(version) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateBudget)
	defer cancel()
	if !update.Check(ctx, exe) {
		return
	}
	fmt.Fprintln(os.Stderr, "◉ applying codehamr update...")
	if err := update.Apply(ctx, exe); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ update failed: %v\n", err)
		if os.IsPermission(err) {
			fmt.Fprintln(os.Stderr, "  tip: rerun with sudo, or reinstall with PREFIX=$HOME/.local")
		}
		return
	}
	// Re-launch the new binary. reExec is platform-split: unix execve (same
	// PID) vs. Windows spawn-and-wait. CODEHAMR_NO_UPDATE_CHECK=1 stops the
	// replacement run from re-checking its own freshly-written hash. On
	// reExec failure we fall through to the old in-memory binary.
	//
	// os.Setenv overwrites in place so os.Environ() carries exactly one entry;
	// append(os.Environ(), …) would leave a pre-existing user-set value first,
	// and Unix execve resolves os.Getenv to the FIRST match, silently defeating
	// the guard if someone exported CODEHAMR_NO_UPDATE_CHECK to a non-"1" value.
	os.Setenv("CODEHAMR_NO_UPDATE_CHECK", "1")
	if err := reExec(exe, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ re-exec failed: %v (continuing with previous version)\n", err)
	}
}

// mustCwd returns the working directory or exits 1, called only where
// there's nothing sensible to recover to.
func mustCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("codehamr: %v", err)
	}
	return cwd
}

// applyEnvOverrides folds runtime env vars into cfg. CODEHAMR_URL overrides
// the active profile's URL (devcontainers / CI), held on a non-serialised
// field so it never round-trips into config.yaml on Save.
func applyEnvOverrides(cfg *config.Config) {
	if envURL := os.Getenv("CODEHAMR_URL"); envURL != "" {
		cfg.URLOverride = envURL
	}
}
