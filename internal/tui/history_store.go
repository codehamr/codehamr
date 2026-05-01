package tui

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
)

// Prompt history persists across restarts in .codehamr/history alongside
// the project's other state, so recall is per-project (cd-stable) and
// /clear can wipe it as part of the project-scoped reset. One
// strconv-quoted entry per line keeps the format dumb (cat-friendly),
// handles multi-line prompts without a separator, and lets a corrupt line
// be skipped without poisoning the rest of the file.
const (
	historyFileName   = "history"
	historyMaxEntries = 500
)

func historyPath(dir string) string { return filepath.Join(dir, historyFileName) }

// loadPromptHistory returns every saved prompt as a chip-less promptEntry,
// oldest first to match the in-memory append order so historyUp/Down walk
// the same direction whether entries were just typed or came off disk. A
// missing file is the first-run state, not an error.
func loadPromptHistory(dir string) []promptEntry {
	f, err := os.Open(historyPath(dir))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []promptEntry
	sc := bufio.NewScanner(f)
	// One prompt may carry a pasted log of tens of KB; raise the per-line
	// cap well past Scanner's 64KB default so we don't silently drop the
	// tail of a long entry on load.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		v, err := strconv.Unquote(sc.Text())
		if err != nil {
			continue
		}
		out = append(out, promptEntry{display: v})
	}
	return out
}

// appendPromptHistory writes value as one more entry, trimming the file to
// historyMaxEntries so on-disk growth stays bounded. Empty prompts are
// skipped so stray ↵ presses don't pollute recall. Whole-file rewrite is
// fine at this size (500 entries × typical prompt length is tens of KB).
func appendPromptHistory(dir, value string) error {
	if value == "" {
		return nil
	}
	all := loadPromptHistory(dir)
	all = append(all, promptEntry{display: value})
	if len(all) > historyMaxEntries {
		all = all[len(all)-historyMaxEntries:]
	}
	var buf []byte
	for _, e := range all {
		buf = append(buf, strconv.Quote(e.display)...)
		buf = append(buf, '\n')
	}
	return os.WriteFile(historyPath(dir), buf, 0o644)
}

// clearPromptHistory removes the on-disk file so /clear's full-reset
// gesture also wipes prompt recall. A missing file is not an error — the
// caller's intent (an empty history) is already satisfied.
func clearPromptHistory(dir string) error {
	err := os.Remove(historyPath(dir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
