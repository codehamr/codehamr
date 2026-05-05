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
	if len(s.VerifyLog) != 0 || len(s.RecentCalls) != 0 {
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
	s.MissingStreak = 1
	s.RecentCalls = []string{"verify|{}", "bash|{}"}
	r := s.HandleDone("Done.", "===== 1 passed in 0.34s =====")
	if !r.EndLoop {
		t.Fatal("should accept")
	}
	if len(s.VerifyLog) != 0 || len(s.RecentCalls) != 0 ||
		s.RedStreak != 0 || s.MissingStreak != 0 || s.LoopToolThisTurn {
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

func TestS2RepeatDetectVerify(t *testing.T) {
	s := newSession(t)
	args := map[string]any{"command": "pytest -x"}
	// Two prior identical attempts.
	if r := s.NoteToolCall(ToolVerify, args); r.Yield {
		t.Fatalf("call 1 unexpectedly yielded: %+v", r)
	}
	if r := s.NoteToolCall(ToolVerify, args); r.Yield {
		t.Fatalf("call 2 unexpectedly yielded: %+v", r)
	}
	r := s.NoteToolCall(ToolVerify, args)
	if !r.Yield {
		t.Fatalf("3rd identical verify should yield: %+v", r)
	}
	if !strings.Contains(r.UserBlock, "verify call repeated 3×") {
		t.Fatalf("S2 block text: %q", r.UserBlock)
	}
}

func TestS2RepeatDetectBash(t *testing.T) {
	// S2 now applies to ALL tools, not just verify. A bash command
	// repeated 3× with identical args yields just like verify.
	s := newSession(t)
	args := map[string]any{"cmd": "ls /tmp"}
	for i := 0; i < 2; i++ {
		if r := s.NoteToolCall("bash", args); r.Yield {
			t.Fatalf("bash call %d unexpectedly yielded", i+1)
		}
	}
	r := s.NoteToolCall("bash", args)
	if !r.Yield {
		t.Fatalf("3rd identical bash should yield: %+v", r)
	}
	if !strings.Contains(r.UserBlock, "bash call repeated 3×") {
		t.Fatalf("S2 bash block text: %q", r.UserBlock)
	}
}

func TestS2RepeatDetectWriteFile(t *testing.T) {
	// Identical write_file (same path + same content) 3× yields. A
	// content tweak between attempts produces a different canonical
	// key and slips past the gate (legitimate iteration).
	s := newSession(t)
	args := map[string]any{"path": "/tmp/foo.txt", "content": "hello"}
	for i := 0; i < 2; i++ {
		if r := s.NoteToolCall("write_file", args); r.Yield {
			t.Fatalf("write_file call %d unexpectedly yielded", i+1)
		}
	}
	r := s.NoteToolCall("write_file", args)
	if !r.Yield {
		t.Fatalf("3rd identical write_file should yield: %+v", r)
	}
}

func TestS2DifferentArgsAllowed(t *testing.T) {
	s := newSession(t)
	if r := s.NoteToolCall("bash", map[string]any{"cmd": "ls a"}); r.Yield {
		t.Fatal("first call yielded unexpectedly")
	}
	if r := s.NoteToolCall("bash", map[string]any{"cmd": "ls b"}); r.Yield {
		t.Fatal("different args should not trigger S2")
	}
	if r := s.NoteToolCall("bash", map[string]any{"cmd": "ls c"}); r.Yield {
		t.Fatalf("third distinct command should not trigger S2: %+v", r)
	}
}

func TestS2DifferentToolsAllowed(t *testing.T) {
	// Same args under different tool names are different keys.
	s := newSession(t)
	args := map[string]any{"x": 1}
	for _, name := range []string{"bash", "write_file", "verify"} {
		if r := s.NoteToolCall(name, args); r.Yield {
			t.Fatalf("%s should not trigger S2 with mixed-tool history: %+v", name, r)
		}
	}
}

func TestS2KeyOrderIndependent(t *testing.T) {
	// JSON canonicalization sorts map keys, so {"a":1,"b":2} and
	// {"b":2,"a":1} collapse to the same S2 key. Without this, the
	// model could trivially evade S2 by reordering args.
	s := newSession(t)
	a := map[string]any{"a": 1, "b": 2}
	b := map[string]any{"b": 2, "a": 1}
	s.NoteToolCall("bash", a)
	s.NoteToolCall("bash", b)
	r := s.NoteToolCall("bash", a)
	if !r.Yield {
		t.Fatalf("logically-identical args (different insertion order) must collapse to one key: %+v", r)
	}
}

func TestS2MalformedArgsFallback(t *testing.T) {
	// json.Marshal returns an error for NaN/Inf; the callKey fallback
	// must still produce a stable key so identical malformed calls
	// still fold together for S2.
	s := newSession(t)
	bad := map[string]any{"v": jsonUnmarshalable{}}
	s.NoteToolCall("bash", bad)
	s.NoteToolCall("bash", bad)
	r := s.NoteToolCall("bash", bad)
	if !r.Yield {
		t.Fatalf("malformed-args fallback must still detect repeats: %+v", r)
	}
}

// jsonUnmarshalable is a value that breaks encoding/json so callKey
// exercises its fallback branch. We use a func type because functions
// are unmarshallable; chan and complex would do too.
type jsonUnmarshalable struct{}

func (jsonUnmarshalable) MarshalJSON() ([]byte, error) {
	return nil, errBadJSON
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

var errBadJSON = sentinelErr("bad json")

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

func TestS4MissingLoopTool(t *testing.T) {
	s := newSession(t)
	r := s.EnsureLoopTool()
	if r.Yield {
		t.Fatal("first missing-loop-tool shouldn't yield (S4)")
	}
	if r.ToolPayload == "" {
		t.Fatal("S4 should return nudge payload")
	}
	if s.MissingStreak != 1 {
		t.Fatalf("MissingStreak=%d, want 1", s.MissingStreak)
	}
}

func TestS5ConsecutiveMissingYields(t *testing.T) {
	s := newSession(t)
	for i := 0; i < MaxMissingStreak-1; i++ {
		r := s.EnsureLoopTool()
		if r.Yield {
			t.Fatalf("missing #%d unexpectedly yielded", i+1)
		}
	}
	r := s.EnsureLoopTool()
	if !r.Yield {
		t.Fatalf("S5 should fire on %dth: %+v", MaxMissingStreak, r)
	}
	if !strings.Contains(r.UserBlock, "verify/done/ask") {
		t.Fatalf("S5 block text: %q", r.UserBlock)
	}
	if s.MissingStreak != 0 {
		t.Fatalf("S5 should reset MissingStreak, got %d", s.MissingStreak)
	}
}

func TestMissingStreakResetsOnLoopTool(t *testing.T) {
	s := newSession(t)
	s.MissingStreak = 2
	// Any of the loop handlers should reset.
	s.RecordVerify("ls", "ok", 0, false)
	if s.MissingStreak != 0 {
		t.Fatalf("verify should reset MissingStreak, got %d", s.MissingStreak)
	}

	s.MissingStreak = 2
	s.HandleAsk("Is this the right approach?")
	if s.MissingStreak != 0 {
		t.Fatalf("ask should reset MissingStreak, got %d", s.MissingStreak)
	}

	s.MissingStreak = 2
	s.RecordVerify("ls", fixedGreen, 0, false)
	s.HandleDone("Done.", "===== 1 passed in 0.34s =====")
	if s.MissingStreak != 0 {
		t.Fatalf("done should reset MissingStreak, got %d", s.MissingStreak)
	}
}

func TestAfterUserMessageClearsState(t *testing.T) {
	s := newSession(t)
	s.RecordVerify("pytest", fixedGreen, 0, false)
	s.NoteToolCall("bash", map[string]any{"cmd": "ls"})
	s.MissingStreak = 2
	s.RedStreak = 1
	s.AfterUserMessage()
	if s.MissingStreak != 0 || s.RedStreak != 0 || len(s.VerifyLog) != 0 || len(s.RecentCalls) != 0 {
		t.Fatalf("AfterUserMessage didn't clear state: %+v", *s)
	}
}

func TestBeginTurnResetsLoopToolFlag(t *testing.T) {
	s := newSession(t)
	s.LoopToolThisTurn = true
	// RecentCalls is deliberately not reset by BeginTurn — repeats
	// straddling a turn boundary should still count.
	s.NoteToolCall("bash", map[string]any{"cmd": "ls"})
	preCalls := len(s.RecentCalls)
	s.BeginTurn()
	if s.LoopToolThisTurn {
		t.Fatal("LoopToolThisTurn should reset")
	}
	if len(s.RecentCalls) != preCalls {
		t.Fatalf("BeginTurn must not touch RecentCalls (got len=%d, want %d)",
			len(s.RecentCalls), preCalls)
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

func TestRecentCallsRingTrimmed(t *testing.T) {
	s := newSession(t)
	for i := 0; i < MaxRecentCalls+3; i++ {
		// Distinct args so S2 doesn't fire — we want to test ring-eviction,
		// not repeat-detection.
		s.NoteToolCall("bash", map[string]any{"cmd": "echo", "i": i})
	}
	if len(s.RecentCalls) != MaxRecentCalls {
		t.Fatalf("RecentCalls len=%d, want %d", len(s.RecentCalls), MaxRecentCalls)
	}
}

func TestS2WindowEvictionAllowsOldRepeats(t *testing.T) {
	// Once the ring rolls past an old key, it no longer counts. This
	// matters: a model that genuinely needs to retry a command after
	// many other calls have happened in between is not "in a loop".
	s := newSession(t)
	args := map[string]any{"cmd": "make test"}
	s.NoteToolCall("bash", args)
	for i := 0; i < MaxRecentCalls; i++ {
		s.NoteToolCall("bash", map[string]any{"cmd": "echo", "i": i})
	}
	// Original "make test" has rolled out of the window. New attempt
	// should be fine.
	if r := s.NoteToolCall("bash", args); r.Yield {
		t.Fatalf("call past window-eviction must not yield: %+v", r)
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
