package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile writes content bytes exactly to path. Parent directories are
// created as needed. Matches the bash convention: errors come back as part
// of the output string, never as a Go error the caller must unwrap — so the
// model sees the failure the same way it sees a non-zero bash exit.
func WriteFile(path, content string) string {
	if path == "" {
		return "(empty path)"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Sprintf("(mkdir error: %v)", err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path)
}

// WriteFileSchema is the OpenAI tool definition for write_file — the second
// local tool every profile exposes, sitting next to bash. The description
// nudges the model toward write_file for any non-trivial file write, so the
// heredoc-quoting failure mode stops happening.
func WriteFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        WriteFileName,
			"description": "Write content bytes to a file at path. Creates parent directories. Overwrites existing files. Use this instead of bash heredocs for multi line content or content with single quotes, dollar signs, or backticks — no shell quoting issues.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Exact bytes to write to the file.",
					},
				},
				"required": []string{"path", "content"},
			},
		},
	}
}
