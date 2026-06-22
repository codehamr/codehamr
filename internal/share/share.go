// Package share uploads a session HTML to a secret GitHub gist via the gh CLI
// and returns a shareable viewer URL.
//
// Two viewer modes:
//   - Default: htmlpreview.github.io, which fetches the gist's raw HTML file
//     and renders it inline. Requires the raw URL, so the gist owner login is
//     extracted from the gist URL gh prints.
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

// extractOwner pulls the login segment between "gist.github.com/" and the ID.
func extractOwner(gistURL string) string {
	s := strings.TrimSuffix(gistURL, "/")
	parts := strings.Split(s, "/")
	// ["https:", "", "gist.github.com", "<owner>", "<id>"]
	if len(parts) >= 5 {
		return parts[len(parts)-2]
	}
	return ""
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
	// Default: htmlpreview.github.io needs the raw gist URL.
	owner := extractOwner(gistURL)
	if owner == "" {
		// Fallback: link the gist itself if we can't build the raw URL.
		return gistURL
	}
	rawURL := fmt.Sprintf("https://gist.githubusercontent.com/%s/%s/raw/", owner, gistID)
	return "https://htmlpreview.github.io/?" + rawURL
}
