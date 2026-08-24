package tools

import (
	"fmt"
	"os"
	"strings"
)

// EditFile replaces old_string with new_string in path. old_string must match
// EXACTLY ONCE; otherwise the file is untouched and an error string is returned
// so the model sees the failure and reacts, same convention as bash/WriteFile.
//
// Empty old_string is rejected (no anchor, every position matches);
// old_string == new_string is rejected as a no-op turn-waster.
func EditFile(path, oldString, newString string) string {
	if path == "" {
		return "(empty path)"
	}
	if oldString == "" {
		return "(empty old_string)"
	}
	if oldString == newString {
		return "(no change: old_string equals new_string)"
	}
	// Same guard as ReadFile: open(2) on a FIFO blocks forever and Ctrl+C
	// can't unblock it, leaking the tool goroutine. Stat never blocks.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Sprintf("(read error: %s is not a regular file)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	content := string(raw)
	n := strings.Count(content, oldString)
	if n == 0 {
		// A near-miss that differs only in whitespace (wrong indentation, tabs vs
		// spaces) is the most common edit_file failure for an LLM. Name it so the
		// model fixes the bytes instead of burning retries toward the failure nudge.
		// Detection only; never auto-apply a fuzzy match, or the exact-match-once
		// safety the caller relies on is gone.
		if differsOnlyInWhitespace(content, oldString) {
			return fmt.Sprintf("(not found: no exact match in %s - a block there differs only in whitespace (indentation/tabs/newlines); copy the exact bytes, including indentation)", path)
		}
		return fmt.Sprintf("(not found: old_string does not appear in %s - read the exact bytes back with read_file before retrying, don't retype them from memory)", path)
	}
	if n > 1 {
		return fmt.Sprintf("(ambiguous: old_string appears %d times - provide more context to make it unique)", n)
	}
	// strings.Count only counts non-overlapping occurrences, so a self-
	// overlapping old_string ("==" in "a === b") passes n == 1 yet matches at
	// two positions with different results. Catch the overlapping second match
	// so the exactly-once guarantee holds.
	if idx := strings.Index(content, oldString); strings.Contains(content[idx+1:], oldString) {
		return "(ambiguous: old_string overlaps itself - provide more context to make it unique)"
	}
	updated := strings.Replace(content, oldString, newString, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("edited %s: -%d +%d bytes", path, len(oldString), len(newString))
}

// differsOnlyInWhitespace reports whether oldString matches content at exactly
// one spot once every whitespace run is collapsed: i.e. the sole mismatch is
// indentation/tabs/newlines. Bounded with spaces so a match can't straddle a
// token boundary and mislabel an unrelated near-miss.
func differsOnlyInWhitespace(content, oldString string) bool {
	norm := func(s string) string { return " " + strings.Join(strings.Fields(s), " ") + " " }
	return strings.Count(norm(content), norm(oldString)) == 1
}

// EditFileSchema is the OpenAI tool definition for edit_file. The description
// steers the model toward edit_file over write_file for small changes so it
// stops rewriting whole documents to fix a typo.
func EditFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        EditFileName,
			"description": "Surgically replace a single occurrence of old_string with new_string in an existing file. old_string must appear EXACTLY ONCE - include enough surrounding context to make it unique. Prefer this over write_file for any change to an existing file short of a full rewrite. To change several places, put several edit_file calls in the SAME message: they run in order against the file on disk, so they compose. Errors (not found, ambiguous, file missing) come back in the result string, same as bash.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path. Relative paths resolve against the working directory.",
					},
					"old_string": map[string]any{
						"type":        "string",
						"description": "Exact substring to find. Must be non-empty and appear exactly once.",
					},
					"new_string": map[string]any{
						"type":        "string",
						"description": "Replacement string. Empty deletes the match.",
					},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		},
	}
}
