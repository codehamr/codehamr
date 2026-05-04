// Package gysd is codehamr's single-mode loop controller. The agent works
// with bash and write_file as today; every turn must end with exactly one
// of three loop tools — verify, done, or ask — and this package owns the
// state machine that enforces it.
//
// The package never executes subprocesses itself. The TUI runs the bash
// command for a verify call (so the goroutine model stays clean) and hands
// the result back via RecordVerify. All other Handle* functions are pure
// state mutations on a single goroutine.
//
// Failure mode this package solves: local LLMs claim "done" without proof.
// done.evidence must be a verbatim substring of a green verify run in this
// loop — Orchestrator-checked, not model-checked. No surface for lying.
package gysd

import (
	"fmt"
	"strings"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Tool names. Centralised so schemas, dispatcher, and tests never drift.
const (
	ToolVerify = "verify"
	ToolDone   = "done"
	ToolAsk    = "ask"
)

// Schranken thresholds. Tightened over several design rounds — every value
// here is load-bearing, see data/gysd.md §5 for rationale and reset rules.
const (
	MaxVerifyLog        = 30                // S1: verify cap per loop
	MaxRecentCommands   = 5                 // S2: window
	RepeatTriggerCount  = 3                 // S2: 3rd identical command yields
	MaxRedStreak        = 3                 // S3: 3 consecutive reds yields
	MaxToolCallsPerTurn = 25                // S4
	MaxTurnDuration     = 10 * time.Minute  // S5
	MaxS6Streak         = 3                 // S7: 3 consecutive non-loop turns
	DefaultTimeout      = 60 * time.Second  // verify default
	MaxTimeout          = 600 * time.Second // verify hard cap = 10min, matches S5
	MaxOutputBytes      = 1 << 20           // 1 MB cap on stored verify output
	HeadTailBytes       = 200 * 1024        // when capped: first+last 200kB each
	MinEvidenceLen      = 20                // done.evidence min length
	MinQuestionLen      = 8                 // ask.question min length after trim
)

// VerifyEntry is one stored verify outcome. Output is ANSI-stripped and
// capped at MaxOutputBytes; the full uncapped form is never retained.
type VerifyEntry struct {
	Command string
	Output  string
	Green   bool
}

// Session is the per-loop state. One instance lives on tui.Model. Reset on
// successful done, /clear, or after a yield+user-message round.
type Session struct {
	VerifyLog        []VerifyEntry
	RedStreak        int
	RecentCommands   []string
	S6Streak         int
	ToolCallsTurn    int
	TurnStart        time.Time
	LoopToolThisTurn bool
}

// Result is the only thing handlers return to the TUI. The TUI inspects
// fields in priority order: EndLoop > Yield > ToolPayload. Zero-value means
// "nothing to do" and the TUI continues its normal flow.
type Result struct {
	ToolPayload  string // becomes a role:tool message content
	EndLoop      bool   // accepted done — final summary, end loop
	Yield        bool   // turn ends, UserBlock printed, await next user msg
	UserBlock    string // shown in scrollback when Yield=true
	FinalSummary string // shown when EndLoop=true
}

// IsLoopTool reports whether a tool name is one this package handles. The
// TUI uses this to short-circuit dispatch (verify/done/ask never go to bash).
func IsLoopTool(name string) bool {
	switch name {
	case ToolVerify, ToolDone, ToolAsk:
		return true
	}
	return false
}

// BeginTurn resets per-turn counters. Called by the TUI whenever a fresh
// LLM round starts (user submit, plan-style nudge, etc.).
func (s *Session) BeginTurn() {
	s.ToolCallsTurn = 0
	s.TurnStart = time.Now()
	s.LoopToolThisTurn = false
}

// NoteToolCall is called by the TUI before dispatching any tool. Bumps the
// per-turn counter and yields when S4 is exceeded — the synthetic UserBlock
// is what the user sees in scrollback when the loop pauses.
func (s *Session) NoteToolCall() Result {
	s.ToolCallsTurn++
	if s.ToolCallsTurn > MaxToolCallsPerTurn {
		return Result{
			Yield: true,
			UserBlock: fmt.Sprintf(
				"⚠ GYSD: %d tool calls in one turn without verify/done/ask — yielding.",
				s.ToolCallsTurn),
		}
	}
	return Result{}
}

// PreVerify validates command, clamps the timeout, and checks S1/S2. If
// run==false the caller emits the embedded Result and skips bash entirely;
// if run==true it executes bash with the returned timeout and then calls
// RecordVerify.
func (s *Session) PreVerify(command string, timeoutSec int) (run bool, timeout time.Duration, result Result) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false, 0, Result{ToolPayload: "verify rejected: command empty."}
	}

	// Clamp timeout to [1s, 600s]. timeoutSec==0 means default.
	switch {
	case timeoutSec <= 0:
		timeout = DefaultTimeout
	case time.Duration(timeoutSec)*time.Second > MaxTimeout:
		timeout = MaxTimeout
	default:
		timeout = time.Duration(timeoutSec) * time.Second
	}

	// S1: verify cap per loop.
	if len(s.VerifyLog) >= MaxVerifyLog {
		return false, 0, Result{
			Yield: true,
			UserBlock: fmt.Sprintf(
				"⚠ GYSD: %d verifies without `done` — yielding. What should I do?",
				len(s.VerifyLog)),
		}
	}

	// S2: identical command 3× in last MaxRecentCommands. matches counts
	// prior occurrences; this attempt would be the (matches+1)-th. Yield
	// when it would be the 3rd, i.e. matches >= RepeatTriggerCount-1.
	matches := 0
	for _, prev := range s.RecentCommands {
		if prev == cmd {
			matches++
		}
	}
	if matches >= RepeatTriggerCount-1 {
		var lastOutput string
		for i := len(s.VerifyLog) - 1; i >= 0; i-- {
			if s.VerifyLog[i].Command == cmd {
				lastOutput = s.VerifyLog[i].Output
				break
			}
		}
		return false, 0, Result{
			Yield: true,
			UserBlock: fmt.Sprintf(
				"⚠ GYSD: same verify command tried %d×. Try a different approach or /clear.\n\nLast output:\n%s",
				RepeatTriggerCount, chmctx.Truncate(lastOutput)),
		}
	}

	return true, timeout, Result{}
}

// RecordVerify is called by the TUI after a verify subprocess completes.
// Stores the entry, updates RedStreak, checks S3. canceled==true means the
// turnCtx was canceled (user Ctrl+C) — no log entry, no streak bump.
func (s *Session) RecordVerify(command, output string, exitCode int, canceled bool) Result {
	s.LoopToolThisTurn = true
	s.S6Streak = 0
	if canceled {
		return Result{ToolPayload: output + "\n(cancelled)"}
	}

	stripped := stripANSI(output)
	capped := capOutput(stripped)
	green := exitCode == 0

	// S2 ring: append, drop oldest beyond window.
	s.RecentCommands = append(s.RecentCommands, command)
	if len(s.RecentCommands) > MaxRecentCommands {
		s.RecentCommands = s.RecentCommands[len(s.RecentCommands)-MaxRecentCommands:]
	}

	// FIFO log.
	s.VerifyLog = append(s.VerifyLog, VerifyEntry{
		Command: command,
		Output:  capped,
		Green:   green,
	})
	if len(s.VerifyLog) > MaxVerifyLog {
		s.VerifyLog = s.VerifyLog[len(s.VerifyLog)-MaxVerifyLog:]
	}

	if green {
		s.RedStreak = 0
	} else {
		s.RedStreak++
		if s.RedStreak >= MaxRedStreak {
			block := s.buildRedStreakBlock()
			s.RedStreak = 0
			return Result{Yield: true, UserBlock: block}
		}
	}

	payload := chmctx.Truncate(capped) + fmt.Sprintf("\n(exit: %d)", exitCode)
	return Result{ToolPayload: payload}
}

// buildRedStreakBlock walks VerifyLog newest-first and quotes the most
// recent MaxRedStreak red entries for the user. Without this the user
// would see only "3 reds in a row" with no actionable detail.
func (s *Session) buildRedStreakBlock() string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠ GYSD: %d consecutive red verifies — yielding.\n", MaxRedStreak)
	n := 0
	for i := len(s.VerifyLog) - 1; i >= 0 && n < MaxRedStreak; i-- {
		e := s.VerifyLog[i]
		if !e.Green {
			fmt.Fprintf(&b, "\n— `%s`:\n%s\n", e.Command, chmctx.Truncate(e.Output))
			n++
		}
	}
	b.WriteString("\nUser: how should I proceed?")
	return b.String()
}

// HandleDone validates evidence and either ends the loop or rejects with
// a tool-result that keeps the turn running (model can verify and retry).
func (s *Session) HandleDone(summary, evidence string) Result {
	s.LoopToolThisTurn = true
	s.S6Streak = 0
	if strings.TrimSpace(summary) == "" {
		return Result{ToolPayload: "done rejected: summary empty."}
	}
	if len(evidence) < MinEvidenceLen {
		return Result{ToolPayload: fmt.Sprintf(
			"done rejected: evidence must be >= %d chars verbatim from a green verify.",
			MinEvidenceLen)}
	}
	for _, e := range s.VerifyLog {
		if e.Green && strings.Contains(e.Output, evidence) {
			final := strings.TrimSpace(summary)
			s.Reset()
			return Result{EndLoop: true, FinalSummary: final}
		}
	}
	return Result{ToolPayload: "done rejected: evidence does not match any green verify in this loop. Run a relevant verify first and quote its output."}
}

// HandleAsk yields to the user with the model's question. Trimmed length
// must be at least MinQuestionLen.
func (s *Session) HandleAsk(question string) Result {
	s.LoopToolThisTurn = true
	s.S6Streak = 0
	q := strings.TrimSpace(question)
	if len(q) < MinQuestionLen {
		return Result{ToolPayload: fmt.Sprintf(
			"ask rejected: question too short (>= %d chars after trim).",
			MinQuestionLen)}
	}
	return Result{Yield: true, UserBlock: q}
}

// EnsureLoopTool is called by the TUI after a turn closes with no pending
// tool calls and the assistant message is recorded. If a loop tool ran in
// the turn: zero-value, TUI ends the turn normally. If not: nudge as
// user-turn (ToolPayload), or yield (S7) when S6Streak hit MaxS6Streak.
func (s *Session) EnsureLoopTool() Result {
	if s.LoopToolThisTurn {
		s.S6Streak = 0
		return Result{}
	}
	s.S6Streak++
	if s.S6Streak >= MaxS6Streak {
		streak := s.S6Streak
		s.S6Streak = 0
		return Result{
			Yield: true,
			UserBlock: fmt.Sprintf(
				"⚠ GYSD: model didn't end with verify/done/ask for %d turns — yielding.",
				streak),
		}
	}
	return Result{
		ToolPayload: "End every turn with verify, done, or ask.",
	}
}

// TurnExpired reports whether the per-turn wall-clock budget is gone. The
// TUI calls this on every stream event; first true triggers a turn cancel.
func (s *Session) TurnExpired() bool {
	if s.TurnStart.IsZero() {
		return false
	}
	return time.Since(s.TurnStart) > MaxTurnDuration
}

// AfterUserMessage clears per-loop state when the user replies to a yield.
// Counters reset; new user message starts a fresh sub-loop. Distinct from
// Reset (full wipe) so the difference between "loop completed" and "loop
// continues with new context" stays explicit.
func (s *Session) AfterUserMessage() {
	s.VerifyLog = nil
	s.RedStreak = 0
	s.RecentCommands = nil
	s.S6Streak = 0
}

// Reset wipes the whole Session. Used after accepted done and /clear.
func (s *Session) Reset() {
	*s = Session{}
}

// capOutput truncates oversized verify output before storing. Keeps first
// HeadTailBytes + last HeadTailBytes around a marker. Mirrors the bash-tool
// output-truncation principle (PROMPT_SYS.md:120) so the model can re-run
// a more targeted check if it needs the missing middle.
func capOutput(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	head := s[:HeadTailBytes]
	tail := s[len(s)-HeadTailBytes:]
	marker := fmt.Sprintf("\n[…truncated by GYSD: full output was %d bytes…]\n", len(s))
	return head + marker + tail
}
