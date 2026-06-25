// Package tools holds the local executors (bash, read_file, write_file,
// edit_file) and the router that dispatches assistant tool calls by name.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Wire-format tool names. One source so schema, router, and inline-status
// switch can't drift apart.
const (
	BashName      = "bash"
	WriteFileName = "write_file"
	EditFileName  = "edit_file"
	ReadFileName  = "read_file"
)

// maxBashTimeoutSeconds caps the per-call timeout_seconds the model can
// request, a backstop against runaway loops (`sleep 99999`, `while true`)
// that would otherwise tie up the turn until Ctrl+C.
const maxBashTimeoutSeconds = 3600

// Bash runs one shell command through /bin/sh -c and returns combined
// stdout+stderr. Non-zero exit is not an error; the model sees the failure
// and reacts.
//
// A pre-cancelled parent (Ctrl+C raced the dispatch) returns "(cancelled)"
// before the blank-command check, so a trivially-cancelled call isn't
// mistaken for a valid empty-args invocation.
func Bash(parent context.Context, command string, timeout time.Duration) string {
	if parent.Err() != nil {
		return "(cancelled)"
	}
	if strings.TrimSpace(command) == "" {
		return "(empty command)"
	}
	ctxT, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxT, "/bin/sh", "-c", command)
	// Shell gets its own process group + a Cancel that kills the whole group
	// on cancel/timeout (Unix; no-op on Windows). Without it, backgrounded
	// children (`cmd &`) outlive the parent shell and leak.
	setProcessGroup(cmd)
	// Cap the wait for stdout/stderr pipes to close after /bin/sh exits.
	// Backgrounded children inherit those pipe fds, so without this
	// CombinedOutput blocks for the full timeout even though the shell is gone.
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		switch {
		case ctxT.Err() == context.DeadlineExceeded:
			return s + fmt.Sprintf("\n(timeout after %s)", timeout)
		case parent.Err() == context.Canceled || ctxT.Err() == context.Canceled:
			// User Ctrl+C; name it rather than leak "signal: killed" noise.
			return s + "\n(cancelled)"
		case errors.Is(err, exec.ErrWaitDelay):
			// Shell exited 0; err is non-nil only because a backgrounded child
			// held the pipes past WaitDelay, not a failure. Return output as-is
			// so it isn't mislabeled with a spurious (exit: ...). After the
			// cancel/timeout cases so those signals win over a coincident delay.
			return s
		default:
			// Exit errors go into the output, exactly what the model needs.
			s += fmt.Sprintf("\n(exit: %v)", err)
		}
	}
	return s
}

// BashSchema is the OpenAI tool definition for bash, exposed by every profile.
func BashSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        BashName,
			"description": "Run a shell command inside the dev container. Combined stdout+stderr is returned. Use targeted commands (grep, head, tail) to avoid the 6k truncation.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{
						"type":        "string",
						"description": "The shell command to execute.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional per call timeout in seconds. Default 120, hard capped at 3600. Raise for commands you expect to run long (pytest on large suites, docker build, DB migrations).",
					},
				},
				"required": []string{"cmd"},
			},
		},
	}
}

// Execute runs a tool call and returns the (possibly truncated) result ready
// to be appended to the conversation as a `tool` message.
func Execute(parent context.Context, call chmctx.ToolCall) chmctx.Message {
	raw := runRaw(parent, call)
	return chmctx.Message{
		Role:       chmctx.RoleTool,
		Content:    chmctx.Truncate(raw),
		ToolCallID: call.ID,
		ToolName:   call.Name,
	}
}

func runRaw(parent context.Context, call chmctx.ToolCall) string {
	// A truncated/oversized tool call leaves llm.resolve()'s _parse_error
	// sentinel where real args should be. Without this guard the call falls
	// through to an empty path/cmd and returns a misleading "(empty path)",
	// hiding that the server cut the arguments at its output-token limit, the
	// failure that makes a model re-emit the same too-large write for minutes.
	// Name the real cause and the recovery so it self-corrects in one step.
	if msg, ok := call.Arguments["_parse_error"].(string); ok {
		return fmt.Sprintf("(tool arguments were not valid JSON: %s, most likely the "+
			"content was too large and the server truncated the call at its output-token "+
			"limit. Do NOT retry the same whole-file write. Build the file in chunks with "+
			"bash heredoc append: `cat > path <<'EOF'` … `EOF` for the first part, then "+
			"repeated `cat >> path <<'EOF'` … `EOF` for each next part, then verify with "+
			"`wc -c path`.)", msg)
	}
	switch call.Name {
	case BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		// Default 2m, overridable per call up to 1h. Clamp seconds BEFORE the
		// Duration multiply: 1e18 would overflow int64 into a negative duration,
		// and 0.5 would truncate to 0 and cancel before the shell runs, so
		// floor at 1.
		timeout := 2 * time.Minute
		if secs, ok := call.Arguments["timeout_seconds"].(float64); ok && secs > 0 {
			secs = min(max(secs, 1), maxBashTimeoutSeconds)
			timeout = time.Duration(secs) * time.Second
		}
		return Bash(parent, cmd, timeout)
	case WriteFileName:
		path, _ := call.Arguments["path"].(string)
		content, _ := call.Arguments["content"].(string)
		return WriteFile(path, content)
	case EditFileName:
		path, _ := call.Arguments["path"].(string)
		oldString, _ := call.Arguments["old_string"].(string)
		newString, _ := call.Arguments["new_string"].(string)
		return EditFile(path, oldString, newString)
	case ReadFileName:
		path, _ := call.Arguments["path"].(string)
		return ReadFile(path)
	default:
		return fmt.Sprintf("(unknown tool: %s)", call.Name)
	}
}

// InlineStatus is the one-liner the TUI prints per tool call.
func InlineStatus(call chmctx.ToolCall) string {
	switch call.Name {
	case BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		return "▶ bash: " + firstLine(cmd)
	case WriteFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ write_file: " + path
	case EditFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ edit_file: " + path
	case ReadFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ read_file: " + path
	default:
		// Fall back to the first non-empty string arg.
		for _, v := range call.Arguments {
			if s, ok := v.(string); ok && s != "" {
				return fmt.Sprintf("▶ %s: %s", call.Name, firstLine(s))
			}
		}
		return "▶ " + call.Name
	}
}

func firstLine(s string) string {
	// IndexAny over both separators, not just '\n', so a CR or CRLF first line
	// (old-mac or pasted command) cuts at the same point instead of leaking a
	// bare '\r' that would yank the status line's cursor back. Mirrors
	// llm.firstLine.
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		// Snap the cut back to a rune boundary, else a multi-byte char gets
		// split and the printed status carries invalid UTF-8 to the terminal.
		cut := 117
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}
