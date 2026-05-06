// Package ctx owns conversation messages, tool output truncation, and the
// newest-first packing rule.
package ctx

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"name,omitempty"`
}

// Tokens is the char/4 heuristic from the spec — good enough for budgeting.
func Tokens(s string) int { return (len(s) + 3) / 4 }

func (m Message) Tokens() int {
	n := Tokens(m.Content) + Tokens(m.Thinking)
	for _, tc := range m.ToolCalls {
		n += Tokens(tc.Name)
		for k, v := range tc.Arguments {
			n += Tokens(k) + Tokens(fmt.Sprint(v))
		}
	}
	return n + 8
}

const (
	ToolOutputCap = 6000
	ToolHeadTail  = 2000
	FixedSystem   = 3000
	FixedTools    = 1500
)

// ResponseReserve is the slice of the context window Budget keeps free for
// the model's response. Scales as ctxSize/8 so big-context reasoning models
// get adequate room: at 262k that's 32k — exactly the Qwen3-class
// thinking-mode default — at 1M it's 125k. Floored at 8k so 32k/64k-class
// profiles keep the legacy reserve and don't collapse history to nothing.
func ResponseReserve(ctxSize int) int {
	if r := ctxSize / 8; r > 8000 {
		return r
	}
	return 8000
}

// Truncate collapses oversized tool outputs to first 2k + last 2k tokens.
// Inputs at or under 6k tokens pass through unchanged. The 8k/8k byte
// slices never overlap because total > 6k tokens means len(out) > 24k
// bytes, comfortably more than the 16k we keep. Slice boundaries are
// snapped to the nearest valid UTF-8 rune start so a non-ASCII output
// (umlauts, box drawing, emoji) never produces invalid bytes mid-stream.
func Truncate(out string) string {
	total := Tokens(out)
	if total <= ToolOutputCap {
		return out
	}
	limit := ToolHeadTail * 4
	head := runeBoundaryDown(out, limit)
	tail := runeBoundaryUp(out, len(out)-limit)
	marker := fmt.Sprintf("\n───── truncated: %d tokens total · showing first %d + last %d ─────\n",
		total, ToolHeadTail, ToolHeadTail)
	return out[:head] + marker + out[tail:]
}

// runeBoundaryDown walks i left until out[i] starts a rune, so out[:i]
// never ends mid-sequence. Safe for i == len(out).
func runeBoundaryDown(out string, i int) int {
	if i >= len(out) {
		return len(out)
	}
	for i > 0 && !utf8.RuneStart(out[i]) {
		i--
	}
	return i
}

// runeBoundaryUp walks i right until out[i] starts a rune, so out[i:]
// never starts mid-sequence. Safe for i <= 0.
func runeBoundaryUp(out string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(out) && !utf8.RuneStart(out[i]) {
		i++
	}
	return i
}

// PackResult records what Pack did so the caller can show the drop banner.
type PackResult struct {
	Messages []Message
	Dropped  int
	Kept     int
}

// Pack keeps whole messages newest-first until the budget is full, then
// returns them in chronological order. The newest message is always kept
// even if it alone exceeds the budget.
//
// A second pass drops tool messages whose tool_call_id never appears in a
// preceding assistant's tool_calls: OpenAI-compatible servers reject a tool
// message that can't be paired with its request, so when packing cuts an
// assistant.tool_calls ancestor off the top of the window we'd rather lose
// the stale response than the entire chat request.
func Pack(history []Message, budget int) PackResult {
	kept := make([]Message, 0, len(history))
	used := 0
	// walk newest to oldest
	for i := len(history) - 1; i >= 0; i-- {
		cost := history[i].Tokens()
		if len(kept) > 0 && used+cost > budget {
			break
		}
		kept = append(kept, history[i])
		used += cost
	}
	slices.Reverse(kept)
	kept = dropOrphanTools(kept)
	return PackResult{
		Messages: kept,
		Dropped:  len(history) - len(kept),
		Kept:     len(kept),
	}
}

// dropOrphanTools removes tool messages whose tool_call_id has no matching
// assistant.tool_calls entry earlier in the kept slice. Happens when
// budget-trimming cut the assistant that issued the call; sending the
// orphaned tool response on its own returns a 400 from every OpenAI-
// compatible backend ("tool message without preceding tool_calls").
func dropOrphanTools(kept []Message) []Message {
	seen := map[string]bool{}
	out := kept[:0]
	for _, m := range kept {
		if m.Role == RoleAssistant {
			for _, tc := range m.ToolCalls {
				seen[tc.ID] = true
			}
		}
		if m.Role == RoleTool && !seen[m.ToolCallID] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Budget subtracts the fixed reservations from the total context size.
func Budget(ctxSize int) int {
	b := ctxSize - FixedSystem - FixedTools - ResponseReserve(ctxSize)
	if b < 0 {
		return 0
	}
	return b
}
