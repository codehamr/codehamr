// Package tools holds the bash executor plus the tool-router that dispatches
// assistant tool calls to either bash or a configured MCP server.
package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Wire-format tool names. Centralised so the schema, the router switch, and
// the inline-status switch can never drift apart. Mirrors the pattern in the
// plan package (plan.ToolSubmitPlan etc.).
const (
	BashName      = "bash"
	WriteFileName = "write_file"
)

// maxBashTimeoutSeconds caps the per-call timeout the model can request via
// timeout_seconds. Backstop against runaway loops (`sleep 99999`,
// `while true`) that would otherwise tie up the turn until Ctrl+C. Lifted
// out of runRaw so it's discoverable next to the schema.
const maxBashTimeoutSeconds = 3600

// Bash runs a single shell command through /bin/sh -c. Output (stdout +
// stderr combined) is returned as a single string. Non-zero exit is not an
// error — the model gets to see the failure and react.
func Bash(parent context.Context, command string, timeout time.Duration) string {
	if strings.TrimSpace(command) == "" {
		return "(empty command)"
	}
	ctxT, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxT, "/bin/sh", "-c", command)
	// Bounds how long we wait for stdout/stderr pipes to close after /bin/sh
	// exits. Without this, `cmd &` backgrounding leaks pipe fds to the
	// grandchild and CombinedOutput blocks for the full timeout even though
	// the shell itself is already gone.
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		switch {
		case ctxT.Err() == context.DeadlineExceeded:
			return s + fmt.Sprintf("\n(timeout after %s)", timeout)
		case parent.Err() == context.Canceled || ctxT.Err() == context.Canceled:
			// Parent cancellation (user Ctrl+C) is a first-class signal —
			// spell it out rather than leaking "signal: killed" noise.
			return s + "\n(cancelled)"
		default:
			// Exit errors surface as part of the output — exactly what the model needs.
			s += fmt.Sprintf("\n(exit: %v)", err)
		}
	}
	return s
}

// BashSchema is the OpenAI tool definition for bash — the single local tool
// every profile exposes to the model.
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

// Dispatch resolves a tool call. bash runs locally; anything else is forwarded
// to the MCP registry (if the caller provides one).
type MCPDispatcher interface {
	Call(ctx context.Context, server, tool string, args map[string]any) (string, error)
	Has(tool string) (server string, ok bool)
}

// Execute runs a tool call and returns the (possibly truncated) result ready
// to be appended to the conversation as a `tool` message.
func Execute(parent context.Context, call chmctx.ToolCall, disp MCPDispatcher) chmctx.Message {
	raw := runRaw(parent, call, disp)
	return chmctx.Message{
		Role:       chmctx.RoleTool,
		Content:    chmctx.Truncate(raw),
		ToolCallID: call.ID,
		ToolName:   call.Name,
	}
}

func runRaw(parent context.Context, call chmctx.ToolCall, disp MCPDispatcher) string {
	switch call.Name {
	case BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		// Default 2 minute timeout, overridable per call up to 1 hour via
		// timeout_seconds. Clamp the integer seconds BEFORE the Duration
		// multiplication: a float like 1e18 would overflow int64 on
		// `* time.Second` and wrap to a negative duration; a fractional
		// value like 0.5 would truncate to 0 and cancel before the shell
		// runs. The schema declares `integer` so floor at 1.
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
	default:
		if disp == nil {
			return fmt.Sprintf("(unknown tool: %s)", call.Name)
		}
		srv, ok := disp.Has(call.Name)
		if !ok {
			return fmt.Sprintf("(unknown tool: %s)", call.Name)
		}
		out, err := disp.Call(parent, srv, call.Name, call.Arguments)
		if err != nil {
			// MCP servers carry the actual diagnostic in the response
			// content array even when isError=true; dropping it leaves
			// the model with a useless "(tool error: tool reported error)"
			// and no idea what to react to. Append the body when the
			// dispatcher returned one alongside the error.
			if out != "" {
				return fmt.Sprintf("(tool error: %v)\n%s", err, out)
			}
			return fmt.Sprintf("(tool error: %v)", err)
		}
		return out
	}
}

// InlineStatus is the one-liner the TUI prints per tool call (spec §Tool Calls).
func InlineStatus(call chmctx.ToolCall) string {
	switch call.Name {
	case BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		return "▶ bash: " + firstLine(cmd)
	case WriteFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ write_file: " + path
	default:
		// try to pluck a meaningful arg (first string value)
		for _, v := range call.Arguments {
			if s, ok := v.(string); ok && s != "" {
				return fmt.Sprintf("▶ %s: %s", call.Name, firstLine(s))
			}
		}
		return "▶ " + call.Name
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
