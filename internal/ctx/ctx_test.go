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

// TestTruncateSnapsToRuneBoundary: a payload of multi-byte runes (umlauts
// here — 2 bytes each in UTF-8) must not be sliced mid-sequence by
// Truncate's byte-offset cut. The output must remain valid UTF-8.
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
	if r.Kept < 2 || r.Kept > 3 {
		t.Fatalf("kept=%d want 2 or 3", r.Kept)
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

// TestPackDropsOrphanToolMessage: when budget-trimming cuts the assistant
// whose tool_calls spawned a tool message, that orphaned tool message must
// be dropped — otherwise OpenAI-compat servers 400 with "tool message
// without preceding tool_calls".
func TestPackDropsOrphanToolMessage(t *testing.T) {
	fortyX := strings.Repeat("x", 40)
	history := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: fortyX},
		{Role: RoleAssistant, Content: "reply"},
	}
	// Budget tight enough to drop the first assistant but loose enough that
	// the tool message would otherwise survive. Values tuned against the
	// per-message +8 overhead in Message.Tokens().
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

// TestPackKeepsPairedToolMessage: when both the assistant and its tool
// response fit in the budget, the pair stays intact — we must not regress
// and drop healthy pairs.
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
	// 65k: ctxSize/8 = 8192, just above the 8k floor.
	if got := Budget(65536); got != 65536-3000-1500-8192 {
		t.Fatalf("budget wrong at 65k: %d", got)
	}
	// 262k: ctxSize/8 = 32768, matches Qwen3 thinking-mode default.
	if got := Budget(262144); got != 262144-3000-1500-32768 {
		t.Fatalf("budget wrong at 262k: %d", got)
	}
	if Budget(1000) != 0 {
		t.Fatal("budget must floor at 0")
	}
}

// TestResponseReserveScales pins the reserve curve: floor active until
// ctxSize/8 crosses 8k, then linear. Spot checks the values referenced
// in the docstring so a future "let's tweak the divisor" lands here loud.
func TestResponseReserveScales(t *testing.T) {
	cases := []struct {
		ctxSize int
		want    int
	}{
		{32_768, 8000},     // floor — ctxSize/8 = 4096 < 8000
		{64_000, 8000},     // floor — ctxSize/8 = 8000, not >
		{65_536, 8192},     // just above the floor
		{128_000, 16_000},  // linear
		{262_144, 32_768},  // Qwen3 thinking-mode default
		{1_000_000, 125_000},
	}
	for _, c := range cases {
		if got := ResponseReserve(c.ctxSize); got != c.want {
			t.Errorf("ResponseReserve(%d) = %d, want %d", c.ctxSize, got, c.want)
		}
	}
}
