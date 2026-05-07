package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
)

// TestResizeKeepsPromptInsideTerminal: in inline mode View() renders only
// the live region (optional streaming preview, popover, divider, prompt,
// status bar). Across a resize sequence the View must never claim more
// rows than the terminal — otherwise the textarea would push the status
// bar past the bottom edge or wrap mid-prompt.
func TestResizeKeepsPromptInsideTerminal(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mx := m
	mx.ta.ta.SetValue(strings.Repeat("abc def ghi jkl mno pqr stu vwx yz ", 20))
	mm = mx

	sizes := [][2]int{
		{120, 40}, {60, 20}, {40, 15}, {20, 10},
		{21, 10}, {20, 10}, {21, 10}, {20, 10},
		{20, 8}, {30, 6}, {120, 40},
	}
	for _, s := range sizes {
		w, h := s[0], s[1]
		mm2, _ := mm.Update(tea.WindowSizeMsg{Width: w, Height: h})
		mm = mm2
		got := strings.Count(mm.View(), "\n") + 1
		if got > h {
			t.Errorf("size %dx%d: View has %d rows, must not exceed %d", w, h, got, h)
		}
	}
}

// TestFirstResizeDoesNotClearScreen: on the very first WindowSizeMsg the
// terminal still holds the user's shell output (and whatever else was on
// screen before codehamr launched). Wiping it would feel destructive, so
// the first resize never returns tea.ClearScreen — only the splash gets
// printed into the outbox.
func TestFirstResizeDoesNotClearScreen(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")

	_, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmdYieldsClearScreen(cmd) {
		t.Error("first WindowSizeMsg must not request ClearScreen")
	}
}

// TestSubsequentResizeFlushesAndClears: every subsequent resize is the
// hardening path. Any in-flight streaming preview must be drained into
// terminal scrollback (so the live region shrinks to chrome and old wide
// lines can no longer haunt the next frame), and tea.ClearScreen must be
// returned so bubbletea's renderer re-anchors at (0,0). Without both,
// soft-wrapped fragments of the previous frame stay orphaned above the
// prompt and the cursor-up math drifts on every render.
func TestSubsequentResizeFlushesAndClears(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	// First resize seeds the splash and is exempt from the hardening.
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Inject in-flight streaming content: this is what the user would see
	// rendered live above the prompt mid-turn.
	mx := mm.(Model)
	mx.streaming.WriteString("partial reply line one\nline two\nline three\n")
	mx.phase = phaseStreaming
	mm = mx

	mm2, cmd := mm.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	nm := mm2.(Model)

	if nm.streaming.Len() != 0 {
		t.Errorf("streaming buffer should be empty after resize, got %d bytes", nm.streaming.Len())
	}
	if !cmdYieldsClearScreen(cmd) {
		t.Error("subsequent WindowSizeMsg must return tea.ClearScreen")
	}
}

// TestRedundantResizeIsNoOp: when the terminal sends a WindowSizeMsg that
// reports the same dimensions we already know about (some terminals do
// this on focus events), the hardening path must not fire — clearing the
// screen for a non-event would flicker the viewport for no benefit.
func TestRedundantResizeIsNoOp(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmdYieldsClearScreen(cmd) {
		t.Error("same-size resize must not request ClearScreen")
	}
}

// TestWidenResizeDoesNotClear: when the terminal grows wider, lines from
// the previous frame (which were ≤ old width) all fit inside the new
// width — no soft-wrap, no cursor drift, no orphans. Bubbletea's own
// repaint handles the fresh draw correctly, so the hardening path must
// stay out of it. Otherwise the user would lose recent scrollback context
// every time they widen their window, which is actively user-hostile.
func TestWidenResizeDoesNotClear(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	_, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmdYieldsClearScreen(cmd) {
		t.Error("widening resize must not request ClearScreen")
	}
}

// TestHeightOnlyResizeDoesNotClear: changing the terminal height alone
// (width stays the same) cannot induce the soft-wrap that breaks
// bubbletea's cursor math, because no line gets wider than its own
// container. The hardening path is reserved for width narrowing.
func TestHeightOnlyResizeDoesNotClear(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	if cmdYieldsClearScreen(cmd) {
		t.Error("height-only resize must not request ClearScreen")
	}
}

// cmdYieldsClearScreen reports whether cmd (or any leaf of a tea.Batch)
// produces the unexported clearScreenMsg that bubbletea's renderer reacts
// to. We compare via reflect because the message type is unexported.
func cmdYieldsClearScreen(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	target := reflect.TypeOf(tea.ClearScreen())
	msg := cmd()
	if reflect.TypeOf(msg) == target {
		return true
	}
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return false
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		if reflect.TypeOf(c()) == target {
			return true
		}
	}
	return false
}
