package gysd

import (
	"strings"
	"testing"
	"time"
)

// fixedGreen is a multi-line, non-trivial pytest-style output used as
// canonical "real" verify output across tests. Length comfortably exceeds
// MinEvidenceLen so substring quoting is straightforward.
const fixedGreen = "===== test session starts =====\nplatform linux -- Python 3.11\ncollected 1 item\n\ntests/test_x.py::test_y PASSED\n\n===== 1 passed in 0.34s ====="

func newSession(t *testing.T) *Session {
	t.Helper()
	s := &Session{}
	s.BeginTurn()
	return s
}

func TestVerifyGreenStored(t *testing.T) {
	s := newSession(t)
	r := s.RecordVerify("pytest tests/test_x.py", fixedGreen, 0, false)
	if r.Yield || r.EndLoop {
		t.Fatalf("unexpected end state: %+v", r)
	}
	if !s.LoopToolThisTurn {
		t.Fatal("LoopToolThisTurn not set")
	}
	if len(s.VerifyLog) != 1 || !s.VerifyLog[0].Green {
		t.Fatalf("verify log = %+v, want 1 green entry", s.VerifyLog)
	}
	if !strings.Contains(r.ToolPayload, "(exit: 0)") {
		t.Fatalf("payload missing exit suffix: %q", r.ToolPayload)
	}
}

func TestVerifyRedBumpsStreak(t *testing.T) {
	s := newSession(t)
	r := s.RecordVerify("pytest", "FAILED ... AssertionError", 1, false)
	if r.Yield {
		t.Fatalf("first red shouldn't yield: %+v", r)
	}
	if s.RedStreak != 1 {
		t.Fatalf("RedStreak=%d, want 1", s.RedStreak)
	}
	if s.VerifyLog[0].Green {
		t.Fatal("entry should be red")
	}
}

func TestVerifyTimeoutCountedAsRed(t *testing.T) {
	// Caller (TUI) decides exit-code semantics for timeout; gysd just
	// records what it's given. Convention: timeout = nonzero exit.
	s := newSession(t)
	s.RecordVerify("sleep 9999", "(timeout after 60s)", 124, false)
	if s.VerifyLog[0].Green {
		t.Fatal("timeout should be red")
	}
	if s.RedStreak != 1 {
		t.Fatalf("RedStreak=%d, want 1", s.RedStreak)
	}
}

func TestVerifyANSIStripped(t *testing.T) {
	s := newSession(t)
	colored := "\x1b[31mFAIL\x1b[0m: tests/test_x.py — \x1b[1mAssertionError\x1b[0m at line 12 of test_x.py"
	s.RecordVerify("pytest", colored, 1, false)
	stored := s.VerifyLog[0].Output
	if strings.Contains(stored, "\x1b") {
		t.Fatalf("ANSI not stripped: %q", stored)
	}
	if !strings.Contains(stored, "FAIL") || !strings.Contains(stored, "AssertionError") {
		t.Fatalf("text content lost: %q", stored)
	}
}

func TestVerifyCancelDoesNotBumpStreak(t *testing.T) {
	s := newSession(t)
	s.RedStreak = 1
	r := s.RecordVerify("pytest", "(cancelled)", 0, true)
	if !strings.Contains(r.ToolPayload, "cancelled") {
		t.Fatalf("payload missing cancel notice: %q", r.ToolPayload)
	}
	if s.RedStreak != 1 {
		t.Fatalf("RedStreak=%d, want unchanged 1", s.RedStreak)
	}
	if len(s.VerifyLog) != 0 {
		t.Fatalf("cancelled verify should not be logged, got %d entries", len(s.VerifyLog))
	}
}

func TestPreVerifyEmptyCommandRejected(t *testing.T) {
	s := newSession(t)
	run, _, r := s.PreVerify("   \t\n", 0)
	if run {
		t.Fatal("empty command should not run")
	}
	if r.Yield {
		t.Fatalf("empty command should reject (not yield): %+v", r)
	}
	if !strings.Contains(r.ToolPayload, "rejected") {
		t.Fatalf("missing rejection: %q", r.ToolPayload)
	}
	if len(s.VerifyLog) != 0 || len(s.RecentCommands) != 0 {
		t.Fatal("empty command should not touch state")
	}
}

func TestPreVerifyTimeoutClamping(t *testing.T) {
	s := newSession(t)
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, DefaultTimeout},
		{-1, DefaultTimeout},
		{30, 30 * time.Second},
		{600, 600 * time.Second},
		{9999, MaxTimeout},
	}
	for _, c := range cases {
		run, got, _ := s.PreVerify("ls", c.in)
		if !run {
			t.Fatalf("ls should run, got false")
		}
		if got != c.want {
			t.Fatalf("PreVerify(timeout=%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestVerifyOutputCapped(t *testing.T) {
	s := newSession(t)
	huge := strings.Repeat("x", MaxOutputBytes+1024)
	s.RecordVerify("find /", huge, 0, false)
	stored := s.VerifyLog[0].Output
	if len(stored) > MaxOutputBytes+1024 {
		t.Fatalf("stored len=%d, expected <= %d-ish", len(stored), MaxOutputBytes+1024)
	}
	if !strings.Contains(stored, "truncated by GYSD") {
		t.Fatalf("missing truncation marker in: %s...", stored[:200])
	}
}

func TestDoneEvidenceTooShort(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	r := s.HandleDone("Fixed the bug.", "too short")
	if r.EndLoop {
		t.Fatal("should reject short evidence")
	}
	if !strings.Contains(r.ToolPayload, "rejected") {
		t.Fatalf("missing rejection: %q", r.ToolPayload)
	}
}

func TestDoneEvidenceNoMatch(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	r := s.HandleDone("All good.", "this does not match anything in the log")
	if r.EndLoop {
		t.Fatal("non-matching evidence should reject")
	}
	if !strings.Contains(r.ToolPayload, "match any green verify") {
		t.Fatalf("wrong rejection text: %q", r.ToolPayload)
	}
}

func TestDoneEvidenceSubstringMatch(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	// Any verbatim 20+ char substring should accept.
	r := s.HandleDone("Tests green.", "===== 1 passed in 0.34s =====")
	if !r.EndLoop {
		t.Fatalf("substring match should end loop: %+v", r)
	}
	if r.FinalSummary != "Tests green." {
		t.Fatalf("FinalSummary=%q", r.FinalSummary)
	}
}

func TestDoneEmptySummaryRejected(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	r := s.HandleDone("   ", "===== 1 passed in 0.34s =====")
	if r.EndLoop {
		t.Fatal("empty summary should reject")
	}
	if !strings.Contains(r.ToolPayload, "summary empty") {
		t.Fatalf("wrong rejection: %q", r.ToolPayload)
	}
}

func TestDoneOnRedVerifyRejected(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 1, false) // red despite content
	r := s.HandleDone("All good.", "===== 1 passed in 0.34s =====")
	if r.EndLoop {
		t.Fatal("done should require a green verify, not red")
	}
}

func TestDoneResetsSession(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	s.RedStreak = 2
	s.S6Streak = 1
	r := s.HandleDone("Done.", "===== 1 passed in 0.34s =====")
	if !r.EndLoop {
		t.Fatal("should accept")
	}
	if len(s.VerifyLog) != 0 || len(s.RecentCommands) != 0 ||
		s.RedStreak != 0 || s.S6Streak != 0 || s.LoopToolThisTurn {
		t.Fatalf("session not fully reset: %+v", *s)
	}
}

func TestAskQuestionTooShort(t *testing.T) {
	s := newSession(t)
	r := s.HandleAsk("ok?")
	if r.Yield {
		t.Fatal("short question should reject (not yield)")
	}
	if !strings.Contains(r.ToolPayload, "too short") {
		t.Fatalf("wrong rejection: %q", r.ToolPayload)
	}
}

func TestAskWhitespaceTooShort(t *testing.T) {
	s := newSession(t)
	r := s.HandleAsk("       ")
	if r.Yield {
		t.Fatal("whitespace-only question should reject")
	}
}

func TestAskValidQuestion(t *testing.T) {
	s := newSession(t)
	r := s.HandleAsk("Should I use JWT or session cookies?")
	if !r.Yield {
		t.Fatalf("valid ask should yield: %+v", r)
	}
	if r.UserBlock != "Should I use JWT or session cookies?" {
		t.Fatalf("UserBlock=%q", r.UserBlock)
	}
}

func TestS1VerifyCap(t *testing.T) {
	s := newSession(t)
	for i := 0; i < MaxVerifyLog; i++ {
		s.RecordVerify("ls", "out", 0, false)
	}
	// MaxVerifyLog-th run was just stored. The next PreVerify should yield.
	run, _, r := s.PreVerify("ls", 0)
	if run {
		t.Fatal("S1 should block run after MaxVerifyLog entries")
	}
	if !r.Yield {
		t.Fatalf("S1 should yield: %+v", r)
	}
	if !strings.Contains(r.UserBlock, "without `done`") {
		t.Fatalf("S1 block text: %q", r.UserBlock)
	}
}

func TestS2RepeatDetect(t *testing.T) {
	s := newSession(t)
	// Two prior identical commands.
	s.RecordVerify("pytest -x", "FAIL", 1, false)
	s.RecordVerify("pytest -x", "FAIL", 1, false)
	// Different commands in between are fine — S2 counts within window.
	run, _, r := s.PreVerify("pytest -x", 0)
	if run {
		t.Fatal("S2 should block 3rd identical command")
	}
	if !r.Yield {
		t.Fatalf("S2 should yield: %+v", r)
	}
	if !strings.Contains(r.UserBlock, "tried 3×") {
		t.Fatalf("S2 block text: %q", r.UserBlock)
	}
}

func TestS2DifferentCommandsAllowed(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest a.py", "ok", 0, false)
	s.RecordVerify("pytest b.py", "ok", 0, false)
	run, _, r := s.PreVerify("pytest c.py", 0)
	if !run {
		t.Fatalf("different commands should not trigger S2: %+v", r)
	}
}

func TestS3RedStreak(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", "FAIL", 1, false)
	r := s.RecordVerify("pytest -v", "FAIL", 1, false)
	if r.Yield {
		t.Fatal("2 reds shouldn't yield yet")
	}
	r = s.RecordVerify("pytest -vv", "FAIL", 1, false)
	if !r.Yield {
		t.Fatalf("3rd consecutive red should yield: %+v", r)
	}
	if !strings.Contains(r.UserBlock, "consecutive red") {
		t.Fatalf("S3 block text: %q", r.UserBlock)
	}
	// After yield, streak should reset for fresh sub-loop.
	if s.RedStreak != 0 {
		t.Fatalf("RedStreak should reset after yield, got %d", s.RedStreak)
	}
}

func TestS3GreenResetsStreak(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", "FAIL", 1, false)
	s.RecordVerify("pytest", "FAIL", 1, false)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	if s.RedStreak != 0 {
		t.Fatalf("green should reset RedStreak, got %d", s.RedStreak)
	}
}

func TestS4ToolCap(t *testing.T) {
	s := newSession(t)
	var last Result
	for i := 0; i < MaxToolCallsPerTurn; i++ {
		last = s.NoteToolCall()
		if last.Yield {
			t.Fatalf("call %d unexpectedly yielded: %+v", i+1, last)
		}
	}
	r := s.NoteToolCall()
	if !r.Yield {
		t.Fatalf("call %d should yield (>%d): %+v", MaxToolCallsPerTurn+1, MaxToolCallsPerTurn, r)
	}
	if !strings.Contains(r.UserBlock, "tool calls in one turn") {
		t.Fatalf("S4 block text: %q", r.UserBlock)
	}
}

func TestS5TurnExpired(t *testing.T) {
	s := newSession(t)
	if s.TurnExpired() {
		t.Fatal("fresh turn shouldn't be expired")
	}
	s.TurnStart = time.Now().Add(-MaxTurnDuration - time.Second)
	if !s.TurnExpired() {
		t.Fatal("turn past MaxTurnDuration should be expired")
	}
}

func TestS6MissingLoopTool(t *testing.T) {
	s := newSession(t)
	r := s.EnsureLoopTool()
	if r.Yield {
		t.Fatal("first S6 shouldn't yield")
	}
	if r.ToolPayload == "" {
		t.Fatal("S6 should return nudge payload")
	}
	if s.S6Streak != 1 {
		t.Fatalf("S6Streak=%d, want 1", s.S6Streak)
	}
}

func TestS7ConsecutiveS6Yields(t *testing.T) {
	s := newSession(t)
	for i := 0; i < MaxS6Streak-1; i++ {
		r := s.EnsureLoopTool()
		if r.Yield {
			t.Fatalf("S6 #%d unexpectedly yielded", i+1)
		}
	}
	r := s.EnsureLoopTool()
	if !r.Yield {
		t.Fatalf("S7 should fire on %dth: %+v", MaxS6Streak, r)
	}
	if !strings.Contains(r.UserBlock, "verify/done/ask") {
		t.Fatalf("S7 block text: %q", r.UserBlock)
	}
	if s.S6Streak != 0 {
		t.Fatalf("S7 should reset S6Streak, got %d", s.S6Streak)
	}
}

func TestS6StreakResetsOnLoopTool(t *testing.T) {
	s := newSession(t)
	s.S6Streak = 2
	// Any of the loop handlers should reset.
	s.RecordVerify("ls", "ok", 0, false)
	if s.S6Streak != 0 {
		t.Fatalf("verify should reset S6Streak, got %d", s.S6Streak)
	}

	s.S6Streak = 2
	s.HandleAsk("Is this the right approach?")
	if s.S6Streak != 0 {
		t.Fatalf("ask should reset S6Streak, got %d", s.S6Streak)
	}

	s.S6Streak = 2
	s.RecordVerify("ls", fixedGreen, 0, false)
	s.HandleDone("Done.", "===== 1 passed in 0.34s =====")
	if s.S6Streak != 0 {
		t.Fatalf("done should reset S6Streak, got %d", s.S6Streak)
	}
}

func TestS6StreakClearedAfterUserMessage(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	s.S6Streak = 2
	s.RedStreak = 1
	s.AfterUserMessage()
	if s.S6Streak != 0 || s.RedStreak != 0 || len(s.VerifyLog) != 0 || len(s.RecentCommands) != 0 {
		t.Fatalf("AfterUserMessage didn't clear state: %+v", *s)
	}
}

func TestBeginTurnResetsPerTurn(t *testing.T) {
	s := newSession(t)
	s.NoteToolCall()
	s.NoteToolCall()
	s.LoopToolThisTurn = true
	preStart := s.TurnStart
	time.Sleep(time.Millisecond)
	s.BeginTurn()
	if s.ToolCallsTurn != 0 {
		t.Fatalf("ToolCallsTurn=%d, want 0", s.ToolCallsTurn)
	}
	if s.LoopToolThisTurn {
		t.Fatal("LoopToolThisTurn should reset")
	}
	if !s.TurnStart.After(preStart) {
		t.Fatal("TurnStart should advance")
	}
}

func TestVerifyLogFIFOEviction(t *testing.T) {
	s := newSession(t)
	// Run MaxVerifyLog + 5 to force eviction.
	for i := 0; i < MaxVerifyLog+5; i++ {
		// PreVerify wouldn't allow past MaxVerifyLog, but RecordVerify itself
		// must still trim if state ever ends up over (defensive). We bypass
		// PreVerify deliberately to test the FIFO contract.
		s.RecordVerify("ls", "out", 0, false)
	}
	if len(s.VerifyLog) != MaxVerifyLog {
		t.Fatalf("VerifyLog len=%d, want %d", len(s.VerifyLog), MaxVerifyLog)
	}
}

func TestRecentCommandsRingTrimmed(t *testing.T) {
	s := newSession(t)
	for i := 0; i < MaxRecentCommands+3; i++ {
		s.RecordVerify("pytest -x foo", "ok", 0, false)
	}
	if len(s.RecentCommands) != MaxRecentCommands {
		t.Fatalf("RecentCommands len=%d, want %d", len(s.RecentCommands), MaxRecentCommands)
	}
}

func TestIsLoopTool(t *testing.T) {
	for _, name := range []string{ToolVerify, ToolDone, ToolAsk} {
		if !IsLoopTool(name) {
			t.Errorf("IsLoopTool(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"bash", "write_file", "submit_plan", ""} {
		if IsLoopTool(name) {
			t.Errorf("IsLoopTool(%q) = true, want false", name)
		}
	}
}

func TestSchemasWellFormed(t *testing.T) {
	for _, schema := range LoopTools() {
		fn, ok := schema["function"].(map[string]any)
		if !ok {
			t.Fatalf("schema missing function: %+v", schema)
		}
		if name, _ := fn["name"].(string); name == "" {
			t.Fatalf("schema name empty: %+v", schema)
		}
		if desc, _ := fn["description"].(string); len(desc) < 40 {
			t.Fatalf("schema description too short: %q", desc)
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok || params["type"] != "object" {
			t.Fatalf("schema parameters malformed: %+v", fn)
		}
	}
}
