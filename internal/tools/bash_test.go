package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestBashEchoesStdout(t *testing.T) {
	out := Bash(context.Background(), "echo hammer && echo time >&2", 5*time.Second)
	if !strings.Contains(out, "hammer") || !strings.Contains(out, "time") {
		t.Fatalf("combined output missing: %q", out)
	}
}

func TestBashNonZeroExitNotFatal(t *testing.T) {
	out := Bash(context.Background(), "false", 5*time.Second)
	if !strings.Contains(out, "exit") {
		t.Fatalf("expected exit marker, got %q", out)
	}
}

func TestBashEmptyCommand(t *testing.T) {
	if Bash(context.Background(), " ", time.Second) != "(empty command)" {
		t.Fatal("empty command handling wrong")
	}
}

func TestBashTimeout(t *testing.T) {
	out := Bash(context.Background(), "sleep 2", 100*time.Millisecond)
	if !strings.Contains(out, "timeout") {
		t.Fatalf("expected timeout marker: %q", out)
	}
}

func TestBashCustomTimeoutHonored(t *testing.T) {
	// A timeout_seconds of 1 should truncate the 3 second sleep.
	// Also verifies the argument flows through runRaw into Bash.
	start := time.Now()
	call := chmctx.ToolCall{
		ID: "t1", Name: "bash",
		Arguments: map[string]any{
			"cmd":             "sleep 3",
			"timeout_seconds": float64(1),
		},
	}
	msg := Execute(context.Background(), call, nil)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("custom timeout ignored; elapsed %s", elapsed)
	}
	if !strings.Contains(msg.Content, "timeout") {
		t.Fatalf("expected timeout marker: %q", msg.Content)
	}
}

func TestBashTimeoutCappedAtOneHour(t *testing.T) {
	// A request for 999999 seconds must be clamped to 3600. We cannot sleep
	// an hour to prove it, so instead verify the call completes quickly with
	// a short command (i.e. no overflow, no panic, honours the happy path).
	call := chmctx.ToolCall{
		ID: "t2", Name: "bash",
		Arguments: map[string]any{
			"cmd":             "echo clamped",
			"timeout_seconds": float64(999999),
		},
	}
	msg := Execute(context.Background(), call, nil)
	if !strings.Contains(msg.Content, "clamped") {
		t.Fatalf("expected echo output: %q", msg.Content)
	}
}

// TestBashTimeoutOverflowClamped: extreme float values must be clamped
// BEFORE the Duration multiplication. `time.Duration(1e18) * time.Second`
// overflows int64 and wraps to a negative deadline, which would make
// context.WithTimeout fire instantly — the command would "succeed" in
// negative time without actually running. Clamping up front avoids the
// trap.
func TestBashTimeoutOverflowClamped(t *testing.T) {
	call := chmctx.ToolCall{
		ID: "t3", Name: "bash",
		Arguments: map[string]any{
			"cmd":             "echo ok",
			"timeout_seconds": float64(1e18),
		},
	}
	msg := Execute(context.Background(), call, nil)
	if !strings.Contains(msg.Content, "ok") {
		t.Fatalf("overflow clamp: expected echo output, got %q", msg.Content)
	}
}

func TestBashBackgroundedChildDoesNotBlock(t *testing.T) {
	// A naked `cmd &` leaks the child's stdout/stderr pipes. Verify that we
	// do not block on those pipes after /bin/sh has exited. This is what
	// caused multi minute stalls in real sessions before WaitDelay was set.
	start := time.Now()
	Bash(context.Background(), "sleep 3 &", 5*time.Second)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bash blocked for %s on backgrounded child's pipes; want <500ms", elapsed)
	}
}

func TestExecuteBashWrapsResult(t *testing.T) {
	call := chmctx.ToolCall{
		ID: "call_1", Name: "bash",
		Arguments: map[string]any{"cmd": "echo hi"},
	}
	msg := Execute(context.Background(), call, nil)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_1" || msg.ToolName != "bash" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.Contains(msg.Content, "hi") {
		t.Fatalf("content missing: %q", msg.Content)
	}
}

// fakeDispatcher implements MCPDispatcher with canned responses so the
// MCP-error path through runRaw can be exercised without spawning a server.
type fakeDispatcher struct {
	hasName string
	out     string
	err     error
}

func (f fakeDispatcher) Has(name string) (string, bool) {
	if name == f.hasName {
		return "fake-server", true
	}
	return "", false
}

func (f fakeDispatcher) Call(_ context.Context, _, _ string, _ map[string]any) (string, error) {
	return f.out, f.err
}

// TestExecuteMCPErrorPreservesContent: when an MCP server returns isError=true
// the diagnostic text travels in the content body alongside the sentinel
// error. Dropping the body left the model staring at "(tool error: tool
// reported error)" with no clue what to react to. The body must travel through
// to the tool-result message.
func TestExecuteMCPErrorPreservesContent(t *testing.T) {
	disp := fakeDispatcher{
		hasName: "lookup",
		out:     "rate limit exceeded · retry in 30s",
		err:     errTool,
	}
	call := chmctx.ToolCall{ID: "x", Name: "lookup"}
	msg := Execute(context.Background(), call, disp)
	if !strings.Contains(msg.Content, "rate limit exceeded") {
		t.Fatalf("MCP error body must travel through to tool result: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "tool error") {
		t.Fatalf("error sentinel must still be present: %q", msg.Content)
	}
}

// TestExecuteMCPErrorEmptyBodyFallsBackToSentinel: a dispatcher returning an
// error with no content body produces the bare "(tool error: ...)" line, no
// stray newlines or empty body indicators.
func TestExecuteMCPErrorEmptyBodyFallsBackToSentinel(t *testing.T) {
	disp := fakeDispatcher{hasName: "lookup", err: errTool}
	call := chmctx.ToolCall{ID: "x", Name: "lookup"}
	msg := Execute(context.Background(), call, disp)
	if strings.Contains(msg.Content, "\n") {
		t.Fatalf("empty-body error should not carry a trailing newline: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "tool error") {
		t.Fatalf("error sentinel missing: %q", msg.Content)
	}
}

var errTool = sentinelErr("tool reported error")

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

func TestExecuteUnknownTool(t *testing.T) {
	call := chmctx.ToolCall{ID: "x", Name: "nope"}
	msg := Execute(context.Background(), call, nil)
	if !strings.Contains(msg.Content, "unknown tool") {
		t.Fatalf("expected unknown-tool error: %q", msg.Content)
	}
}

func TestInlineStatusBash(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{Name: "bash",
		Arguments: map[string]any{"cmd": "ls -la\nrm /tmp/x"}})
	if !strings.HasPrefix(s, "▶ bash: ls -la") || strings.Contains(s, "\n") {
		t.Fatalf("bad inline status: %q", s)
	}
}

func TestInlineStatusGeneric(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{Name: "context7",
		Arguments: map[string]any{"query": "react useEffect"}})
	if !strings.HasPrefix(s, "▶ context7: react") {
		t.Fatalf("bad inline status: %q", s)
	}
}
