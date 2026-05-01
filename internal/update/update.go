// Package update performs a passive, fire-and-forget freshness check against
// the latest GitHub release. It hashes the running executable with sha256 and
// compares that against the row for the current os/arch in the published
// `codehamr_checksums.txt` asset that goreleaser uploads with every release.
// Mismatch = the user's local binary is stale.
//
// The check is a signal only — this package never replaces the binary. It
// exists so the TUI can show a dezent update hint on the splash; applying the
// update is the user's job (re-run install.sh).
//
// Any network hiccup, offline machine, missing asset, or parse glitch returns
// "no update" rather than surfacing an error: a startup banner that shouts
// when the internet is flaky is worse than one that stays quiet. The
// CODEHAMR_NO_UPDATE_CHECK=1 env var is the user escape hatch for air-gapped
// setups or CI runs where a GitHub fetch is unwanted.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// checksumsURL is the "latest" redirect GitHub serves for the goreleaser
// checksums asset. Direct CDN download — no GitHub API call, so no 60/hour
// rate limit to worry about even for users who start many TUI sessions.
const checksumsURL = "https://github.com/codehamr/codehamr/releases/latest/download/codehamr_checksums.txt"

// releaseBase is the stable "latest" redirect for individual binary assets.
// Paired with asset names from assetName() to form the download URL in Apply.
const releaseBase = "https://github.com/codehamr/codehamr/releases/latest/download/"

// fetchTimeout bounds the checksums.txt GET. Matches the TUI's own ping
// budget so a silent network can't extend startup.
const fetchTimeout = 2 * time.Second

// Check compares the local binary's sha256 against the remote asset's
// recorded hash and reports whether they differ. ctx is honoured so a parent
// cancel (Ctrl+C during startup) propagates into the HTTP request. Returns
// false on any failure — see package doc for the rationale.
func Check(ctx context.Context, execPath string) bool {
	if os.Getenv("CODEHAMR_NO_UPDATE_CHECK") == "1" {
		return false
	}
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return false
	}
	local, err := hashFile(execPath)
	if err != nil {
		return false
	}
	remote, err := fetchHash(ctx, asset)
	if err != nil || remote == "" {
		return false
	}
	return !strings.EqualFold(local, remote)
}

// assetName mirrors the name_template in .goreleaser.yaml. Unsupported
// platforms (e.g. freebsd) return ok=false so we skip the check entirely
// instead of probing for an asset that was never built.
func assetName(goos, goarch string) (string, bool) {
	switch goos {
	case "linux":
		// keep as-is
	case "darwin":
		goos = "macos"
	default:
		return "", false
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", false
	}
	return fmt.Sprintf("codehamr-%s-%s", goos, goarch), true
}

// hashFile streams a file through sha256. Used against os.Executable(); a
// ~10MB Go binary hashes in a few ms, so no streaming optimisation needed.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Apply downloads the current platform's binary from the "latest" release
// and atomically replaces execPath with it. Intended to be called at
// startup from main.go — the caller is expected to syscall.Exec afterwards
// so the running process turns into the new binary without an intermediate
// user-visible restart.
//
// The temp file is created in the same directory as execPath so os.Rename
// stays an atomic intra-filesystem move. If the directory is read-only
// (typical for `/usr/local/bin` without sudo), os.CreateTemp fails with
// EACCES — the error is returned verbatim so main.go can print a helpful
// hint about rerunning with sudo or using a user-local PREFIX.
//
// ctx governs the whole download; no http.Client.Timeout is set so the
// caller controls the budget exclusively.
func Apply(ctx context.Context, execPath string) error {
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	tmp, err := os.CreateTemp(filepath.Dir(execPath), ".codehamr-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Belt and braces: a deferred Close on the temp file catches every
	// early-return below without each one needing to spell it out, and
	// the deferred Remove cleans up if anything fails before the rename
	// promotes the temp file. After a successful rename tmpPath no longer
	// exists, so os.Remove returns ENOENT and we ignore it. A Close after
	// an explicit Close on *os.File is harmless.
	defer os.Remove(tmpPath)
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", releaseBase+asset, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: status %d", resp.StatusCode)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpPath, execPath)
}

// fetchHash downloads codehamr_checksums.txt and returns the hash for the
// given asset name. Goreleaser's default manifest format is one line per
// asset, "<hex-sha256>  <filename>" — we match against the last field so any
// future prefix tweak still works.
func fetchHash(ctx context.Context, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", checksumsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("non-200")
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == asset {
			return fields[0], nil
		}
	}
	return "", nil
}
