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

// TestWidthChangeSuppressesView: any width change (narrow OR widen)
// flips suppressView so the renderer's async ticker has nothing to
// commit between SIGWINCH and the settle. View() must return "" while
// suppressed; bubbletea's renderer expands that into one blank row +
// EraseScreenBelow, leaving nothing for soft-wrap reflow to orphan.
// Streaming buffer is preserved (re-wrapped on resume).
func TestWidthChangeSuppressesView(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	mx := mm.(Model)
	mx.streaming.WriteString("partial reply line one\nline two\nline three\n")
	mx.phase = phaseStreaming
	mm = mx

	for _, next := range []int{60, 100} { // narrow then widen — both must suppress
		mm2, cmd := mm.Update(tea.WindowSizeMsg{Width: next, Height: 24})
		nm := mm2.(Model)

		if !nm.suppressView {
			t.Errorf("width change to %d must enable suppressView", next)
		}
		if got := nm.View(); got != "" {
			t.Errorf("View() must return \"\" while suppressed (width=%d), got %q", next, got)
		}
		if nm.streaming.Len() == 0 {
			t.Errorf("streaming buffer must survive width change to %d", next)
		}
		if cmd == nil {
			t.Errorf("width change to %d must schedule a settle tick, got nil cmd", next)
		}
		mm = nm
	}
}

// TestResizeSettleReplaysScrollbackAtNewWidth: when the settle tick
// matches, the model returns a strict tea.Sequence that wipes both
// viewport and scrollback, re-emits the splash at the new width, and
// replays the entire m.scroll transcript. After this every line in
// scrollback was emitted at the current width — no previous-width
// rows soft-wrap into stair-steps, and the splash always matches the
// terminal layout.
func TestResizeSettleReplaysScrollbackAtNewWidth(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	// Seed some history into m.scroll so the settle has something to
	// replay (mimics an earlier user submit + assistant response).
	mx := mm.(Model)
	mx.scroll.WriteString("▌ erkläre kurz bitcoin\nBitcoin ist eine digitale Währung.\n")
	mm = mx

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	nm := mm.(Model)
	if !nm.suppressView {
		t.Fatal("setup precondition: suppressView must be true after width change")
	}

	mm2, settle := nm.Update(resizeSettleMsg{gen: nm.resizeGen})
	nm2 := mm2.(Model)

	if nm2.suppressView {
		t.Error("matching settle must flip suppressView back to false")
	}
	if !cmdYieldsClearScreen(settle) {
		t.Error("settle must include tea.ClearScreen")
	}
	if !cmdYieldsScrollbackErase(settle) {
		t.Error("settle must include eraseScrollback (\\x1b[3J)")
	}
	if !cmdYieldsPrintln(settle) {
		t.Error("settle must include at least one tea.Println — the splash and/or replayed scroll")
	}
	// Exactly two Println leaves: one for the splash, one for the
	// transcript replay. (No outbox content was queued in this test.)
	if n := countPrintlnLeaves(settle); n != 2 {
		t.Errorf("expected 2 tea.Println leaves (splash + scroll replay), got %d", n)
	}
}

// TestStaleResizeSettleIsDiscarded: a settle msg whose gen no longer
// matches m.resizeGen (because a newer resize bumped it after the tick
// was scheduled) must be a complete no-op. Otherwise the chrome would
// flicker back during a slow drag the moment the first tick fires,
// then disappear again on the next narrowing.
func TestStaleResizeSettleIsDiscarded(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	nm := mm.(Model)

	if nm.resizeGen < 2 {
		t.Fatalf("setup precondition: resizeGen must be >= 2 after two narrowings, got %d", nm.resizeGen)
	}

	stale := resizeSettleMsg{gen: nm.resizeGen - 1}
	mm2, cmd := nm.Update(stale)
	nm2 := mm2.(Model)

	if !nm2.suppressView {
		t.Error("stale settle must NOT flip suppressView off")
	}
	if cmd != nil {
		t.Error("stale settle must return nil cmd (no clear, no further work)")
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

// TestWidenResizeAlsoSuppresses: widening changes the splash layout
// (text→art at the wordmark threshold) and may leave previous-narrow
// rows looking out of place at the new width, so the same suppress +
// settle replay path used for narrowing applies. The immediate cmd
// is the debounce tick — the actual clear lands on settle.
func TestWidenResizeAlsoSuppresses(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	mm, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	nm := mm.(Model)

	if !nm.suppressView {
		t.Error("widening must enable suppressView so the eventual settle cleanly replays the new layout")
	}
	if cmd == nil {
		t.Error("widening must schedule a settle tick")
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

// countCmdLeaves invokes cmd (and recurses into the []tea.Cmd payload
// of tea.BatchMsg / tea.sequenceMsg, both unexported but slice-shaped)
// and counts the leaves where match returns true. Used to assert
// what a Sequence emits without importing bubbletea's internal types.
func countCmdLeaves(cmd tea.Cmd, match func(tea.Cmd, tea.Msg) bool) int {
	if cmd == nil {
		return 0
	}
	msg := cmd()
	if match(cmd, msg) {
		return 1
	}
	rv := reflect.ValueOf(msg)
	if rv.Kind() != reflect.Slice {
		return 0
	}
	n := 0
	for i := 0; i < rv.Len(); i++ {
		c, ok := rv.Index(i).Interface().(tea.Cmd)
		if !ok {
			continue
		}
		n += countCmdLeaves(c, match)
	}
	return n
}

func cmdYieldsClearScreen(cmd tea.Cmd) bool {
	target := reflect.TypeOf(tea.ClearScreen())
	return countCmdLeaves(cmd, func(_ tea.Cmd, msg tea.Msg) bool {
		return reflect.TypeOf(msg) == target
	}) > 0
}

func cmdYieldsScrollbackErase(cmd tea.Cmd) bool {
	needlePtr := reflect.ValueOf(eraseScrollback).Pointer()
	return countCmdLeaves(cmd, func(c tea.Cmd, _ tea.Msg) bool {
		return reflect.ValueOf(c).Pointer() == needlePtr
	}) > 0
}

func cmdYieldsPrintln(cmd tea.Cmd) bool {
	return countPrintlnLeaves(cmd) > 0
}

func countPrintlnLeaves(cmd tea.Cmd) int {
	return countCmdLeaves(cmd, func(_ tea.Cmd, msg tea.Msg) bool {
		t := reflect.TypeOf(msg)
		return t != nil && t.Name() == "printLineMessage"
	})
}
