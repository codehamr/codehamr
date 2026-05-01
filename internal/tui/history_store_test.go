package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPromptHistoryRoundTrip: append a few values (including one with a
// newline and one with quotes), reload from disk, expect identical
// payloads in append order. Anchors the contract that the dumb on-disk
// format survives every byte the textarea can submit.
func TestPromptHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := loadPromptHistory(dir); len(got) != 0 {
		t.Fatalf("fresh dir should have 0 entries, got %d", len(got))
	}
	want := []string{"first", "second\nwith newline", `third "quoted"`, "café 🐹"}
	for _, v := range want {
		if err := appendPromptHistory(dir, v); err != nil {
			t.Fatal(err)
		}
	}
	got := loadPromptHistory(dir)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].display != w {
			t.Errorf("entry %d: got %q, want %q", i, got[i].display, w)
		}
	}
}

// TestPromptHistoryCap: exceeding historyMaxEntries drops the oldest, not
// the newest. Without this guarantee the file grows unbounded over a
// project's lifetime.
func TestPromptHistoryCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < historyMaxEntries+50; i++ {
		if err := appendPromptHistory(dir, "p"+strings.Repeat("x", i%3)); err != nil {
			t.Fatal(err)
		}
	}
	got := loadPromptHistory(dir)
	if len(got) != historyMaxEntries {
		t.Fatalf("cap not enforced: %d entries on disk, want %d", len(got), historyMaxEntries)
	}
	// First on-disk entry should correspond to the 50th submitted prompt
	// (oldest 50 dropped) — proves trim is from the head, not the tail.
	wantHead := "p" + strings.Repeat("x", 50%3)
	if got[0].display != wantHead {
		t.Errorf("trim direction wrong: head=%q want %q", got[0].display, wantHead)
	}
}

// TestPromptHistorySkipEmpty: empty submits never reach the file. Stops a
// stray ↵ from polluting the recall list with blanks.
func TestPromptHistorySkipEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := appendPromptHistory(dir, ""); err != nil {
		t.Fatal(err)
	}
	if got := loadPromptHistory(dir); len(got) != 0 {
		t.Errorf("empty prompt should not be saved, got %d", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, historyFileName)); !os.IsNotExist(err) {
		t.Errorf("history file should not exist after only-empty append, err=%v", err)
	}
}

// TestPromptHistoryClear: clearPromptHistory removes the file, and a
// missing file is not an error. Mirrors the /clear semantics.
func TestPromptHistoryClear(t *testing.T) {
	dir := t.TempDir()
	if err := appendPromptHistory(dir, "x"); err != nil {
		t.Fatal(err)
	}
	if err := clearPromptHistory(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, historyFileName)); !os.IsNotExist(err) {
		t.Fatalf("history file should be gone, err=%v", err)
	}
	// Idempotent: clearing an already-clean dir is a no-op.
	if err := clearPromptHistory(dir); err != nil {
		t.Errorf("second clear must be no-op, got %v", err)
	}
}

// TestPromptHistoryCorruptLineSkipped: a malformed line should not
// poison subsequent valid entries. Important because users may edit the
// file by hand and we should not wipe their good entries on a typo.
func TestPromptHistoryCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, historyFileName)
	body := "not a quoted line\n" + `"valid"` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadPromptHistory(dir)
	if len(got) != 1 || got[0].display != "valid" {
		t.Errorf("expected 1 valid entry, got %+v", got)
	}
}
