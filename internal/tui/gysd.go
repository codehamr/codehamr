package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/gysd"
)

// verifyResultMsg carries a completed verify subprocess back to Update so
// gysd.Session.RecordVerify runs on the main goroutine — Session never
// mutates from a tea.Cmd. Includes the original tool-call ID and the
// command we sent to /bin/sh so the result message and ANSI/exit logging
// stay traceable to a single dispatch.
type verifyResultMsg struct {
	callID   string
	callName string
	command  string
	outcome  gysd.RunOutcome
}

// handleGYSDTool routes verify/done/ask to the gysd Session. verify spawns
// a subprocess (async — runs in tea.Cmd, lands as verifyResultMsg);
// done/ask are pure state mutations and apply synchronously.
func (m Model) handleGYSDTool(call chmctx.ToolCall) (tea.Model, tea.Cmd) {
	switch call.Name {
	case gysd.ToolVerify:
		cmdStr, _ := call.Arguments["command"].(string)
		timeoutSec := argInt(call.Arguments, "timeout_seconds")
		run, timeout, r := m.gysd.PreVerify(cmdStr, timeoutSec)
		if !run {
			return m.applyGYSDResult(r, call.ID, call.Name)
		}
		m.phase = phaseRunning
		return m, dispatchVerify(m.turnCtx, call, cmdStr, timeout)

	case gysd.ToolDone:
		summary, _ := call.Arguments["summary"].(string)
		evidence, _ := call.Arguments["evidence"].(string)
		r := m.gysd.HandleDone(summary, evidence)
		return m.applyGYSDResult(r, call.ID, call.Name)

	case gysd.ToolAsk:
		question, _ := call.Arguments["question"].(string)
		r := m.gysd.HandleAsk(question)
		return m.applyGYSDResult(r, call.ID, call.Name)
	}
	// Unreachable — IsLoopTool gates this in dispatchNextTool. Defensive
	// fallthrough so a future tool-name mismatch surfaces visibly.
	m.appendLine(styleError.Render("⚠ unknown gysd tool: " + call.Name))
	m.endTurn()
	return m, nil
}

// applyGYSDResult turns a gysd.Result into a state mutation + tea.Cmd
// pair. Three outcomes: end the loop (accepted done), yield to user
// (rejected for S1-S7 / ask), or feed a tool-result back to the model.
func (m Model) applyGYSDResult(r gysd.Result, callID, callName string) (tea.Model, tea.Cmd) {
	switch {
	case r.EndLoop:
		m.flushStreaming()
		if s := strings.TrimSpace(r.FinalSummary); s != "" {
			m.appendLine(styleOK.Render("✓ " + s))
		}
		m.finalizeTurn()
		m.endTurn()
		return m, nil

	case r.Yield:
		m.flushStreaming()
		if s := strings.TrimSpace(r.UserBlock); s != "" {
			m.appendLine(styleWarn.Render(s))
		}
		m.finalizeTurn()
		m.endTurn()
		return m, nil

	default:
		// Synthetic tool-result — flows through the same toolResultMsg
		// path as a real tool, so the chat loop continues uniformly.
		m.phase = phaseThinking
		return m, syntheticToolResult(r.ToolPayload, callID, callName)
	}
}

// dispatchVerify spawns the verify subprocess in a tea.Cmd. PreVerify has
// already validated and clamped; this just runs.
func dispatchVerify(parent context.Context, call chmctx.ToolCall, command string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		outcome := gysd.RunCommand(parent, command, timeout)
		return verifyResultMsg{
			callID:   call.ID,
			callName: call.Name,
			command:  command,
			outcome:  outcome,
		}
	}
}

// syntheticToolResult builds the closure that fakes a tool-result message
// arriving from a real tool dispatch. The chat loop in Update treats the
// resulting msg identically to a runToolCall response.
func syntheticToolResult(payload, callID, callName string) tea.Cmd {
	return func() tea.Msg {
		return toolResultMsg{Msg: chmctx.Message{
			Role:       chmctx.RoleTool,
			Content:    payload,
			ToolCallID: callID,
			ToolName:   callName,
		}}
	}
}

// applyVerifyResult is called from Update on verifyResultMsg. Renders the
// inline outcome marker for the user (live UX), then calls RecordVerify
// and turns the resulting Result into the appropriate state transition.
// Stale results from a cancelled turn are dropped before any mutation.
func (m Model) applyVerifyResult(msg verifyResultMsg) (tea.Model, tea.Cmd) {
	if !m.phase.active() {
		return m, nil
	}
	m.appendLine(styleDim.Render(verifyOutcomeLine(msg.outcome)))

	r := m.gysd.RecordVerify(
		msg.command,
		msg.outcome.Output,
		msg.outcome.ExitCode,
		msg.outcome.Canceled,
	)
	return m.applyGYSDResult(r, msg.callID, msg.callName)
}

// verifyOutcomeLine renders one indented status line per verify outcome
// so the user can see grün/rot at a glance without waiting for the next
// model response. The first non-blank line of output usually carries the
// pass/fail summary in pytest, cargo, go test, grep, etc. — preferring
// it over a blind tail keeps creative-open output legible too.
func verifyOutcomeLine(o gysd.RunOutcome) string {
	icon := "✓"
	switch {
	case o.Canceled:
		icon = "⊘"
	case o.TimedOut, o.ExitCode != 0:
		icon = "✗"
	}
	snippet := firstNonBlankLine(o.Output)
	if snippet == "" {
		return fmt.Sprintf("  %s (no output, exit %d)", icon, o.ExitCode)
	}
	if len(snippet) > 160 {
		snippet = snippet[:157] + "..."
	}
	return fmt.Sprintf("  %s %s", icon, snippet)
}

// firstNonBlankLine returns the first line of s with non-whitespace
// content. Used by verifyOutcomeLine; pulled out so the truncation logic
// stays linear instead of nested.
func firstNonBlankLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// argInt extracts an integer argument from a tool-call arguments map.
// JSON unmarshalling produces float64 for numbers; returning 0 on missing/
// wrong-type lets callers treat 0 as "use default". Negative values from
// the model are also returned as 0 — gysd.PreVerify clamps anyway.
func argInt(args map[string]any, name string) int {
	v, ok := args[name]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0
		}
		return int(n)
	case int:
		if n < 0 {
			return 0
		}
		return n
	}
	return 0
}
