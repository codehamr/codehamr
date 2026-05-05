// Package gysd is codehamr's single-mode loop controller. "GYSD" is short
// for Get Your Shit Done: one loop, three loop tools (verify, done, ask),
// one rule — show evidence, don't claim. The agent works with bash and
// write_file as today; every turn must end with exactly one of the three
// loop tools, and this package owns the state machine that enforces it.
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
	"encoding/json"
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

// Schranken thresholds. Five deterministic guards, see data/gysd.md §5.
//
// The design is intentionally minimal: per-tool timeouts are model-set
// (bash 1-3600s, verify 1-600s) and Ctrl+C is the universal escape, so
// there is no turn-level wall-clock cap and no tool-call-count cap. The
// remaining schranken target the failure modes those mechanisms cannot
// catch: spamming verifies (S1), repeating identical calls (S2), failing
// the same way (S3), and never landing on a loop tool (S4/S5).
const (
	MaxVerifyLog       = 30                // S1: verify cap per loop
	MaxRecentCalls     = 5                 // S2: window
	RepeatTriggerCount = 3                 // S2: 3rd identical call yields
	MaxRedStreak       = 3                 // S3: 3 consecutive reds yields
	MaxMissingStreak   = 3                 // S5: 3 consecutive non-loop turns
	DefaultTimeout     = 60 * time.Second  // verify default
	MaxTimeout         = 600 * time.Second // verify hard cap (use background-spawn for longer ops)
	MaxOutputBytes     = 1 << 20           // 1 MB cap on stored verify output
	HeadTailBytes      = 200 * 1024        // when capped: first+last 200kB each
	MinEvidenceLen     = 20                // done.evidence min length
	MinQuestionLen     = 8                 // ask.question min length after trim
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
	RecentCalls      []string // canonical "name|json(args)" keys, most-recent at the end (S2)
	MissingStreak    int      // consecutive turns ending without verify/done/ask (S5)
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

// BeginTurn resets the per-turn loop-tool flag. Called by the TUI whenever
// a fresh LLM round starts (user submit, missing-loop-tool nudge, etc.).
// Note that S2's RecentCalls ring deliberately survives across turns — a
// repeat that straddles a turn boundary is still a repeat.
func (s *Session) BeginTurn() {
	s.LoopToolThisTurn = false
}

// NoteToolCall is the S2 (identical-call repeat) gate. Called by the TUI
// before dispatching ANY tool — bash, write_file, verify, done, ask. The
// canonical key is `name|json(args)` with sorted JSON keys (encoding/json
// sorts map keys deterministically), so logically-equal arg sets always
// produce the same key regardless of insertion order.
//
// Yield fires on the RepeatTriggerCount-th identical call within the last
// MaxRecentCalls dispatches. On non-yield the key is appended to the ring
// (oldest evicted past MaxRecentCalls) so the next call sees this attempt.
func (s *Session) NoteToolCall(name string, args map[string]any) Result {
	key := callKey(name, args)
	matches := 0
	for _, prev := range s.RecentCalls {
		if prev == key {
			matches++
		}
	}
	if matches >= RepeatTriggerCount-1 {
		return Result{
			Yield: true,
			UserBlock: fmt.Sprintf(
				"⚠ Stuck — same `%s` call repeated %d×. Tell me what to try differently, or /clear.",
				name, RepeatTriggerCount),
		}
	}
	s.RecentCalls = append(s.RecentCalls, key)
	if len(s.RecentCalls) > MaxRecentCalls {
		s.RecentCalls = s.RecentCalls[len(s.RecentCalls)-MaxRecentCalls:]
	}
	return Result{}
}

// callKey produces a deterministic comparison key for a tool call. JSON
// marshalling sorts map keys alphabetically (encoding/json contract since
// Go 1.12), so {"a":1,"b":2} and {"b":2,"a":1} collapse to the same key.
// On marshal failure (NaN/Inf floats etc.) we fall back to fmt.Sprintf so
// malformed calls still distinguish themselves rather than collapsing into
// one bucket and triggering false S2s.
func callKey(name string, args map[string]any) string {
	if b, err := json.Marshal(args); err == nil {
		return name + "|" + string(b)
	}
	return name + "|" + fmt.Sprintf("%v", args)
}

// PreVerify validates command, clamps the timeout, and checks S1. If
// run==false the caller emits the embedded Result and skips bash entirely;
// if run==true it executes bash with the returned timeout and then calls
// RecordVerify. S2 (identical-call) is enforced upstream by NoteToolCall
// in dispatchNextTool — every tool, not just verify.
func (s *Session) PreVerify(command string, timeoutSec int) (run bool, timeout time.Duration, result Result) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false, 0, Result{ToolPayload: "verify rejected: command empty."}
	}

	// Clamp timeout to [1s, MaxTimeout]. timeoutSec==0 means default.
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
				"⚠ Stuck — %d verify checks without finishing. Tell me what's missing, or /clear.",
				len(s.VerifyLog)),
		}
	}

	return true, timeout, Result{}
}

// RecordVerify is called by the TUI after a verify subprocess completes.
// Stores the entry, updates RedStreak, checks S3. canceled==true means the
// turnCtx was canceled (user Ctrl+C) — no log entry, no streak bump.
func (s *Session) RecordVerify(command, output string, exitCode int, canceled bool) Result {
	s.LoopToolThisTurn = true
	s.MissingStreak = 0
	if canceled {
		return Result{ToolPayload: output + "\n(cancelled)"}
	}

	stripped := stripANSI(output)
	capped := capOutput(stripped)
	green := exitCode == 0

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
	fmt.Fprintf(&b, "⚠ Stuck — %d verify checks failed in a row.\n", MaxRedStreak)
	n := 0
	for i := len(s.VerifyLog) - 1; i >= 0 && n < MaxRedStreak; i-- {
		e := s.VerifyLog[i]
		if !e.Green {
			fmt.Fprintf(&b, "\n— `%s`:\n%s\n", e.Command, chmctx.Truncate(e.Output))
			n++
		}
	}
	b.WriteString("\nWhat should I try differently?")
	return b.String()
}

// HandleDone validates evidence and either ends the loop or rejects with
// a tool-result that keeps the turn running (model can verify and retry).
func (s *Session) HandleDone(summary, evidence string) Result {
	s.LoopToolThisTurn = true
	s.MissingStreak = 0
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
	s.MissingStreak = 0
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
// the turn: zero-value, TUI ends the turn normally (S4 ok). If not: nudge
// as user-turn (ToolPayload), or yield (S5) when MissingStreak hits
// MaxMissingStreak.
func (s *Session) EnsureLoopTool() Result {
	if s.LoopToolThisTurn {
		s.MissingStreak = 0
		return Result{}
	}
	s.MissingStreak++
	if s.MissingStreak >= MaxMissingStreak {
		streak := s.MissingStreak
		s.MissingStreak = 0
		return Result{
			Yield: true,
			UserBlock: fmt.Sprintf(
				"⚠ Stuck — I drifted off the verify/done/ask loop for %d turns. Tell me how to proceed, or /clear.",
				streak),
		}
	}
	return Result{
		ToolPayload: "End every turn with verify, done, or ask.",
	}
}

// AfterUserMessage clears per-loop state when the user replies to a yield.
// Counters reset; new user message starts a fresh sub-loop. Distinct from
// Reset (full wipe) so the difference between "loop completed" and "loop
// continues with new context" stays explicit.
func (s *Session) AfterUserMessage() {
	s.VerifyLog = nil
	s.RedStreak = 0
	s.RecentCalls = nil
	s.MissingStreak = 0
}

// Reset wipes the whole Session. Used after accepted done and /clear.
func (s *Session) Reset() {
	*s = Session{}
}

// capOutput truncates oversized verify output before storing. Keeps first
// HeadTailBytes + last HeadTailBytes around a marker. Mirrors the bash-tool
// output-truncation principle (PROMPT_SYS.md) so the model can re-run
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
