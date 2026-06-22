package share

import (
	"testing"
)

func TestExtractGistID(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://gist.github.com/alice/abc123def4567890123456789012345678abcd", "abc123def4567890123456789012345678abcd"},
		{"https://gist.github.com/bob/fedcba0987654321fedcba0987654321fedcba09", "fedcba0987654321fedcba0987654321fedcba09"},
		{"https://gist.github.com/alice/abc123", ""}, // too short
		{"not a url", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := extractGistID(c.url)
		if got != c.want {
			t.Errorf("extractGistID(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestExtractOwner(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://gist.github.com/alice/abc123def4567890123456789012345678abcd", "alice"},
		{"https://gist.github.com/bob/abc123def4567890123456789012345678abcd/", "bob"},
		{"https://gist.github.com/abc123def4567890123456789012345678abcd", ""}, // no owner segment
	}
	for _, c := range cases {
		got := extractOwner(c.url)
		if got != c.want {
			t.Errorf("extractOwner(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestViewerURLHtmlpreview(t *testing.T) {
	gistURL := "https://gist.github.com/alice/abc123def4567890123456789012345678abcd"
	gistID := "abc123def4567890123456789012345678abcd"
	url := viewerURL(gistURL, gistID)
	want := "https://htmlpreview.github.io/?https://gist.githubusercontent.com/alice/" + gistID + "/raw/"
	if url != want {
		t.Errorf("viewerURL (htmlpreview) = %q, want %q", url, want)
	}
}

func TestViewerURLOverride(t *testing.T) {
	t.Setenv("CODEHAMR_SHARE_VIEWER_URL", "https://codehamr.com/session/")
	gistURL := "https://gist.github.com/alice/abc123def4567890123456789012345678abcd"
	gistID := "abc123def4567890123456789012345678abcd"
	url := viewerURL(gistURL, gistID)
	want := "https://codehamr.com/session/#" + gistID
	if url != want {
		t.Errorf("viewerURL (override) = %q, want %q", url, want)
	}
}

func TestViewerURLFallbackNoOwner(t *testing.T) {
	// When owner can't be parsed, fall back to the gist URL itself.
	url := viewerURL("https://gist.github.com/abc123def4567890123456789012345678abcd", "abc123def4567890123456789012345678abcd")
	if url != "https://gist.github.com/abc123def4567890123456789012345678abcd" {
		t.Errorf("viewerURL fallback = %q, want gist URL", url)
	}
}

func TestCreateGistGhMissing(t *testing.T) {
	// Force gh to not be found by clearing PATH.
	t.Setenv("PATH", "/nonexistent")
	_, err := CreateGist("<html></html>")
	if err != ErrGhNotInstalled {
		t.Errorf("expected ErrGhNotInstalled, got %v", err)
	}
}
