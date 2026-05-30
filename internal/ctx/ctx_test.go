package ctx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTokensHeuristic(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"a":        1,
		"abcd":     1,
		"abcde":    2,
		"12345678": 2,
	}
	for s, want := range cases {
		if got := Tokens(s); got != want {
			t.Errorf("Tokens(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestMessageTokensCountsToolCallArguments pins ToolCall.Arguments accounting
// in Message.Tokens — it feeds the budget on every tool-using turn, yet no
// other test populates Arguments. Asserts the delta so the +8 per-message
// overhead can't mask a dropped Arguments loop.
func TestMessageTokensCountsToolCallArguments(t *testing.T) {
	base := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{Name: "bash"}}}.Tokens()
	withArgs := Message{Role: RoleAssistant, ToolCalls: []ToolCall{
		{Name: "bash", Arguments: map[string]any{"cmd": "echo hello world"}},
	}}.Tokens()
	// args add Tokens("cmd")=1 + Tokens(fmt.Sprint("echo hello world"))=4 = 5.
	if got := withArgs - base; got != 5 {
		t.Fatalf("argument cost = %d, want 5 (Message.Tokens must account for ToolCall.Arguments)", got)
	}
}

func TestTruncateSmallUntouched(t *testing.T) {
	in := strings.Repeat("x", 20000) // 5000 tokens, under 6k cap
	if out := Truncate(in); out != in {
		t.Fatalf("expected no change for small output")
	}
}

func TestTruncateLargeCollapses(t *testing.T) {
	in := strings.Repeat("abcd", 8000) // 32000 chars ~= 8000 tokens
	out := Truncate(in)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
	if Tokens(out) > 2*ToolHeadTail+200 {
		t.Fatalf("truncated output too large: %d tokens", Tokens(out))
	}
	if !strings.HasPrefix(out, in[:100]) {
		t.Fatal("expected head preserved")
	}
	if !strings.HasSuffix(out, in[len(in)-100:]) {
		t.Fatal("expected tail preserved")
	}
}

// TestTruncateSnapsToRuneBoundary: Truncate's byte-offset cut must not slice
// multi-byte runes mid-sequence — output stays valid UTF-8.
func TestTruncateSnapsToRuneBoundary(t *testing.T) {
	in := strings.Repeat("ä", 20000) // 2 bytes each, 40000 bytes total = 10000 tokens
	out := Truncate(in)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got %q", out[:80])
	}
	if !utf8.ValidString(out) {
		t.Fatal("Truncate produced invalid UTF-8 — slice landed mid-rune")
	}
}

func TestPackNewestFirstWhole(t *testing.T) {
	big := strings.Repeat("a", 4*1000) // 1000 tokens
	history := []Message{
		{Role: RoleUser, Content: big},
		{Role: RoleAssistant, Content: big},
		{Role: RoleUser, Content: big},
		{Role: RoleAssistant, Content: big},
	}
	r := Pack(history, 2500)
	// Each message costs Tokens(4000 bytes)+8 = 1008. Budget 2500 keeps newest
	// (1008) and msg3 (2016 <= 2500), breaks before msg2 (3024 > 2500) — so
	// exactly 2, pinning the `used+cost > budget` break against off-by-one.
	if r.Kept != 2 {
		t.Fatalf("kept=%d want exactly 2", r.Kept)
	}
	// last message must always be kept
	if r.Messages[len(r.Messages)-1].Content != big {
		t.Fatal("newest message not preserved")
	}
}

func TestPackAlwaysKeepsNewest(t *testing.T) {
	massive := strings.Repeat("z", 4*10000)
	history := []Message{
		{Role: RoleUser, Content: "small"},
		{Role: RoleUser, Content: massive},
	}
	r := Pack(history, 100)
	if r.Kept != 1 {
		t.Fatalf("expected only newest kept, got %d", r.Kept)
	}
	if r.Messages[0].Content != massive {
		t.Fatal("newest should have been kept even if over budget")
	}
}

// TestPackDropsOrphanToolMessage: if budget-trimming cuts the assistant whose
// tool_calls spawned a tool message, that orphan must be dropped — else
// OpenAI-compat servers 400 with "tool message without preceding tool_calls".
func TestPackDropsOrphanToolMessage(t *testing.T) {
	fortyX := strings.Repeat("x", 40)
	history := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: fortyX},
		{Role: RoleAssistant, Content: "reply"},
	}
	// Tight enough to drop the first assistant, loose enough that the tool
	// message would otherwise survive. Tuned against the +8 per-message overhead.
	r := Pack(history, 30)
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			t.Fatalf("orphan tool message survived pack: %+v", r.Messages)
		}
	}
	if len(r.Messages) == 0 {
		t.Fatal("newest assistant should have survived")
	}
}

// TestPackDropsEmptyIDToolMessages: an empty-ID tool_call must not let
// subsequent empty-ToolCallID tool messages ride through as "paired". An
// unidentifiable tool message can never be paired, so it's always dropped —
// else OpenAI-compat backends 400 on the bare tool message.
func TestPackDropsEmptyIDToolMessages(t *testing.T) {
	history := []Message{
		// Malformed assistant with a missing tool_call id — server bug.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "", Name: "bash"}}},
		// Looks paired via the empty ID; must be dropped anyway.
		{Role: RoleTool, ToolCallID: "", Content: "from empty1"},
		// Clearly orphan — nothing to pair with.
		{Role: RoleTool, ToolCallID: "", Content: "TRULY ORPHAN"},
		{Role: RoleAssistant, Content: "final"},
	}
	r := Pack(history, 100000)
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			t.Fatalf("empty-ID tool message survived pack: %+v (full kept set: %+v)", m, r.Messages)
		}
	}
}

// TestPackKeepsPairedToolMessage: a healthy assistant+tool pair that fits the
// budget stays intact — don't regress into dropping good pairs.
func TestPackKeepsPairedToolMessage(t *testing.T) {
	history := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
		{Role: RoleAssistant, Content: "done"},
	}
	r := Pack(history, 10000)
	if len(r.Messages) != 3 {
		t.Fatalf("all 3 messages should be kept, got %d: %+v", len(r.Messages), r.Messages)
	}
}

func TestBudget(t *testing.T) {
	// Reference the constants directly so a future FixedSystem/FixedTools tweak
	// doesn't trip a magic-number mismatch absent a real regression.
	// 65k: ctxSize/8 = 8192, just above the 8k floor.
	if got := Budget(65536); got != 65536-FixedSystem-FixedTools-8192 {
		t.Fatalf("budget wrong at 65k: %d", got)
	}
	// 262k: ctxSize/8 = 32768, matches Qwen3 thinking-mode default.
	if got := Budget(262144); got != 262144-FixedSystem-FixedTools-32768 {
		t.Fatalf("budget wrong at 262k: %d", got)
	}
	if Budget(1000) != 0 {
		t.Fatal("budget must floor at 0")
	}
}

// TestResponseReserveScales pins the reserve curve: floored until ctxSize/8
// crosses 8k, then linear. A divisor tweak lands here loud.
func TestResponseReserveScales(t *testing.T) {
	cases := []struct {
		ctxSize int
		want    int
	}{
		{32_768, 8000},    // floor — ctxSize/8 = 4096 < 8000
		{64_000, 8000},    // floor — ctxSize/8 = 8000, not >
		{65_536, 8192},    // just above the floor
		{128_000, 16_000}, // linear
		{262_144, 32_768}, // Qwen3 thinking-mode default
		{1_000_000, 125_000},
	}
	for _, c := range cases {
		if got := ResponseReserve(c.ctxSize); got != c.want {
			t.Errorf("ResponseReserve(%d) = %d, want %d", c.ctxSize, got, c.want)
		}
	}
}
