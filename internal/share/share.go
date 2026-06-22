// Package share uploads a session HTML to a secret GitHub gist via the gh CLI
// and returns a shareable viewer URL.
//
// Two viewer modes:
//   - Default: gisthost.github.io, Simon Willison's maintained fork of
//     gistpreview. It fetches the gist via the GitHub API and renders it with
//     document.write(), so only the gist ID is needed (no owner/raw URL).
//   - Override: CODEHAMR_SHARE_VIEWER_URL (e.g. "https://codehamr.com/session/")
//     makes viewerURL append "#<gistID>" to that base, matching pi.dev's
//     convention for a self-hosted viewer that fetches the gist by ID.
package share

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Result is the outcome of a successful gist upload.
type Result struct {
	GistURL string // https://gist.github.com/<user>/<id>
	GistID  string // <id>
	ViewURL string // the shareable viewer URL
}

var gistIDRE = regexp.MustCompile(`[a-f0-9]{20,}`)

// ErrGhNotInstalled is returned when the gh CLI binary is not on PATH.
var ErrGhNotInstalled = errors.New("GitHub CLI (gh) is not installed - get it from https://cli.github.com/")

// ErrGhNotLoggedIn is returned when gh is installed but not authenticated.
var ErrGhNotLoggedIn = errors.New("GitHub CLI is not logged in - run 'gh auth login' first")

// CreateGist writes htmlContent as a single-file secret gist via `gh gist
// create --public=false`, then builds the viewer URL. No temp file needed: gh
// reads content from stdin when the filename is "-".
func CreateGist(htmlContent string) (Result, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return Result{}, ErrGhNotInstalled
	}

	// Check auth status first for a cleaner error than the gist create failure.
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		return Result{}, ErrGhNotLoggedIn
	}

	// `gh gist create --public=false -` reads from stdin and prints the gist
	// URL to stdout. Using stdin avoids a temp file and a race on cleanup.
	cmd := exec.Command("gh", "gist", "create", "--public=false", "-")
	cmd.Stdin = strings.NewReader(htmlContent)
	out, err := cmd.Output()
	if err != nil {
		ee := &exec.ExitError{}
		if errors.As(err, &ee) {
			return Result{}, fmt.Errorf("gh gist create failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return Result{}, fmt.Errorf("gh gist create failed: %w", err)
	}

	gistURL := strings.TrimSpace(string(out))
	if gistURL == "" {
		return Result{}, errors.New("gh gist create produced no URL")
	}

	gistID := extractGistID(gistURL)
	if gistID == "" {
		return Result{}, fmt.Errorf("could not parse gist ID from %q", gistURL)
	}

	return Result{
		GistURL: gistURL,
		GistID:  gistID,
		ViewURL: viewerURL(gistURL, gistID),
	}, nil
}

// extractGistID pulls the trailing hex ID from a gist URL like
// https://gist.github.com/user/abc123...
func extractGistID(gistURL string) string {
	id := gistURL
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if !gistIDRE.MatchString(id) {
		return ""
	}
	return id
}

// viewerURL builds the shareable preview URL for the gist.
func viewerURL(gistURL, gistID string) string {
	if base := strings.TrimSpace(os.Getenv("CODEHAMR_SHARE_VIEWER_URL")); base != "" {
		// Self-hosted viewer convention (pi.dev style): base + "#<id>"
		if !strings.HasSuffix(base, "#") {
			base += "#"
		}
		return base + gistID
	}
	// Default: gisthost.github.io only needs the gist ID as the query string.
	// (Fork of gistpreview.github.io; handles Substack URL mangling and
	// truncated files via the GitHub Gist API + document.write.)
	if gistID == "" {
		return gistURL
	}
	return "https://gisthost.github.io/?" + gistID
}
