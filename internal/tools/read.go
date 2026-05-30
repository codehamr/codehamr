package tools

import (
	"fmt"
	"os"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// ReadFile returns the contents of path, truncated to the same 6k-token head+
// tail budget as every other tool output (chmctx.Truncate). Robust file reads
// without bash quoting / heredoc / `cat` games: the model gets exact bytes,
// not a shell-mangled approximation. Matches the bash/write/edit convention —
// filesystem errors come back as part of the output string (never a Go error
// the caller unwraps), so the model sees a missing file the same way it sees a
// non-zero bash exit and reacts.
func ReadFile(path string) string {
	if path == "" {
		return "(empty path)"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	return chmctx.Truncate(string(raw))
}

// ReadFileSchema is the OpenAI tool definition for read_file — the fourth
// local tool, sitting next to bash/write_file/edit_file. The description
// nudges the model toward read_file over `cat` for reading source so it
// stops piping files through the shell just to look at them.
func ReadFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ReadFileName,
			"description": "Read a file and return its contents. Prefer this over `cat`/`sed` in bash for inspecting a file — no shell quoting, exact bytes. Output over 6k tokens is truncated to first+last 2k; for a slice of a large file use bash with sed/grep/head/tail.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path.",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}
