package tools

import (
	"fmt"
	"os"
	"strings"
)

// EditFile replaces old_string with new_string in path. old_string must match
// EXACTLY ONCE; otherwise the file is untouched and an error string is returned
// so the model sees the failure and reacts — same convention as bash/WriteFile.
//
// Empty old_string is rejected (no anchor — every position matches);
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
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	content := string(raw)
	n := strings.Count(content, oldString)
	if n == 0 {
		return fmt.Sprintf("(not found: old_string does not appear in %s)", path)
	}
	if n > 1 {
		return fmt.Sprintf("(ambiguous: old_string appears %d times — provide more context to make it unique)", n)
	}
	updated := strings.Replace(content, oldString, newString, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("edited %s: -%d +%d bytes", path, len(oldString), len(newString))
}

// EditFileSchema is the OpenAI tool definition for edit_file. The description
// steers the model toward edit_file over write_file for small changes so it
// stops rewriting whole documents to fix a typo.
func EditFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        EditFileName,
			"description": "Surgically replace a single occurrence of old_string with new_string in an existing file. old_string must appear EXACTLY ONCE in the file — include enough surrounding context to make it unique. Prefer this over write_file for any change to an existing file shorter than a full rewrite: small typo fixes, single-line edits, swapping a function body. Errors (not found, ambiguous, file missing) come back as part of the result string, same as bash.",
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
