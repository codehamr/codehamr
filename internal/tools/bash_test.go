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
	msg := Execute(context.Background(), call)
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
	msg := Execute(context.Background(), call)
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
	msg := Execute(context.Background(), call)
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
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_1" || msg.ToolName != "bash" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.Contains(msg.Content, "hi") {
		t.Fatalf("content missing: %q", msg.Content)
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	call := chmctx.ToolCall{ID: "x", Name: "nope"}
	msg := Execute(context.Background(), call)
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
