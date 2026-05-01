// Command codehamr is the lightweight, fast coding agent for the terminal.
package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/mcp"
	"github.com/codehamr/codehamr/internal/tui"
	"github.com/codehamr/codehamr/internal/update"
)

// updateBudget is the total wall-clock cap for the pre-launch auto-update
// step (checksum fetch + binary download + rename). Generous enough for a
// ~10MB Go binary on a slow connection, tight enough that an offline user
// doesn't wait half a minute before the TUI appears.
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

	// Pre-launch auto-update: if the checksum of the running binary differs
	// from the latest release's published sha256, download the new binary,
	// swap it in, and re-exec so the user immediately runs the fresh
	// version. All failures are non-fatal — a flaky network, missing asset,
	// or read-only install dir (typical for /usr/local/bin without sudo) all
	// fall through to launching the old binary unchanged, with a single
	// stderr line so the user knows why it didn't take.
	maybeSelfUpdate()

	cwd := mustCwd()
	cfg, created, err := config.Bootstrap(cwd)
	if err != nil {
		log.Fatalf("codehamr: %v", err)
	}
	if created {
		fmt.Println(".codehamr/ created")
	}
	applyEnvOverrides(cfg)

	// Debug instrumentation: opt-in via `logging: true` in config.yaml.
	// Truncates .codehamr/log.txt and records every chat exchange. Search
	// for `tui.OpenDebugLog` and `dbgWrite` to remove cleanly.
	if cfg.Logging {
		tui.OpenDebugLog(cfg.Dir)
		defer tui.CloseDebugLog()
	}

	mgr := mcp.NewManager()
	// Snapshot enabled MCP servers before launching the spawn goroutine.
	// /plugins toggles write to cfg.MCPServers from the TUI goroutine; if
	// SpawnAll iterated cfg.MCPServers concurrently, Go would panic with
	// "concurrent map read and map write". The snapshot is taken on the
	// main goroutine before any concurrent access can start.
	mcpSnap := maps.Clone(cfg.MCPServers)
	// Spawn MCP servers in the background so the TUI renders immediately.
	// First-run `npx` downloads can take several seconds; without async,
	// startup looks frozen. Tools become available as each server finishes.
	go func() {
		if errs := mgr.SpawnAll(mcpSnap); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintln(os.Stderr, "⚠ mcp:", e)
			}
		}
	}()
	defer mgr.StopAll()

	p := cfg.ActiveProfile()
	client := llm.New(cfg.ActiveURL(), p.LLM, p.Key)

	abs, _ := filepath.Abs(cwd)
	m := tui.New(cfg, mgr, client, abs, version)

	// Inline mode (no AltScreen, no mouse capture): the TUI renders only
	// the prompt + status bar live region at the bottom of the terminal,
	// and pushes everything else into native scrollback via tea.Println.
	// The terminal itself owns mouse-wheel scrolling, PgUp/PgDn, text
	// selection, and copy/paste — exactly like any normal shell session.
	//
	// WithReportFocus turns raw focus-in / focus-out escape sequences
	// (\x1b[I / \x1b[O) into typed tea.FocusMsg / tea.BlurMsg. Without
	// it, VS Code's integrated terminal and similar xterm.js hosts leak
	// those bytes as runes into the textarea on every window switch,
	// inflating the prompt height by "invisible" characters until the UI
	// appears to shift upward. Swallowing the typed msgs in Update
	// prevents the leak entirely.
	if _, err := tea.NewProgram(m, tea.WithReportFocus()).Run(); err != nil {
		log.Fatalf("codehamr: %v", err)
	}
}

func printHelp() {
	fmt.Println(strings.TrimSpace(`
codehamr — a lightweight, fast coding agent for the terminal.

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
  .codehamr/config.yaml — per-project settings

Env:
  CODEHAMR_URL         override the active profile's url at runtime`))
}

// isLocalBuild reports whether the current binary was compiled from a
// working tree rather than pulled from an official release. `go run` keeps
// `main.version` at its "dev" default; `make install` on a dirty tree
// embeds a `-dirty` suffix via `git describe --dirty`. Goreleaser pins a
// clean tag like `v1.2.3`, so released binaries read as non-local and
// continue to self-update.
func isLocalBuild(version string) bool {
	return version == "dev" || strings.HasSuffix(version, "-dirty")
}

// maybeSelfUpdate runs the pre-launch auto-update step. It's a no-op when:
//   - version is "dev" (the `go run` / `make run` default — updating would
//     overwrite the temp binary Go just compiled from local sources with an
//     older release and silently hide unreleased work),
//   - version ends with "-dirty" (locally built from an uncommitted tree via
//     `make install`; same reasoning — respect what the developer just
//     built),
//   - the sha256 of the running binary already matches the published release,
//   - the platform is unsupported (see update.assetName),
//   - the network, CDN, or filesystem refuses.
//
// On success it swaps the binary on disk and re-execs via syscall.Exec so
// the current process becomes the new binary in place — no fork, no child,
// no second "restart". syscall.Exec only returns on error; a successful
// call never comes back.
//
// Any failure past the point of "update is available" prints one short line
// to stderr and returns, letting main() proceed with the old binary.
func maybeSelfUpdate() {
	// Guard against overwriting a locally-built binary with an older
	// release. Without this, `make run` (which is `go run`, version=="dev")
	// would hash its temp `go-build` binary, find it differs from the
	// published checksum, and silently swap in the last release — hiding
	// any unreleased local changes behind an "update applied" banner.
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
	// Re-exec in place. Environ carries CODEHAMR_NO_UPDATE_CHECK=1 on the
	// replacement run so the already-updated child doesn't loop into a
	// second check against its own freshly-written hash.
	env := append(os.Environ(), "CODEHAMR_NO_UPDATE_CHECK=1")
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ re-exec failed: %v (continuing with previous version)\n", err)
	}
}

// mustCwd returns the current working directory or exits 1. Only called from
// top-level command handlers where there is nothing sensible to recover to.
func mustCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("codehamr: %v", err)
	}
	return cwd
}

// applyEnvOverrides folds runtime env vars into cfg. CODEHAMR_URL overrides
// the active profile's URL — useful in devcontainers / CI where the endpoint
// sidecar address isn't known until runtime. The override lives on cfg in a
// non-serialised field so it never round-trips into config.yaml on Save.
func applyEnvOverrides(cfg *config.Config) {
	if envURL := os.Getenv("CODEHAMR_URL"); envURL != "" {
		cfg.URLOverride = envURL
	}
}

