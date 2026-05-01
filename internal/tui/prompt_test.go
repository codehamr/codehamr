package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// newChippablePrompt returns a promptInput sized to a realistic width so the
// underlying textarea's wrap/cursor math behaves like it does in the TUI.
// Chip-related tests don't care about height; width matters because
// SetCursor / LineInfo navigate through the wrapped grid.
func newChippablePrompt() promptInput {
	p := newPromptInput()
	p.SetWidth(80)
	p.SetHeight(20)
	return p
}

// pasteKey builds a bracketed-paste KeyMsg identical to what bubbletea emits
// when the terminal reports a paste via the OSC sequence. Paste=true is the
// flag promptInput keys off of in Update.
func pasteKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

// makePaste returns a string with exactly n lines (n-1 newlines).
func makePaste(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(parts, "\n")
}

// TestSmallPasteStaysInline: a paste below the line threshold is handed to
// the underlying textarea as-is — no chip created, value is the raw paste.
func TestSmallPasteStaysInline(t *testing.T) {
	p := newChippablePrompt()
	small := makePaste(3)
	p, _ = p.Update(pasteKey(small))

	if len(p.spans) != 0 {
		t.Fatalf("small paste must not create chip, got spans=%+v", p.spans)
	}
	if got := p.DisplayValue(); got != small {
		t.Fatalf("display value should equal raw paste, got %q", got)
	}
	if got := p.Value(); got != small {
		t.Fatalf("expanded value should equal raw paste, got %q", got)
	}
}

// TestLargePasteBecomesChip: a paste with ≥pasteChipMinLines lines collapses
// into exactly one chip. DisplayValue shows the label, Value expands back to
// the original content.
func TestLargePasteBecomesChip(t *testing.T) {
	p := newChippablePrompt()
	big := makePaste(pasteChipMinLines + 4)
	p, _ = p.Update(pasteKey(big))

	if len(p.spans) != 1 {
		t.Fatalf("big paste should produce 1 chip, got %d spans", len(p.spans))
	}
	wantLabel := fmt.Sprintf("[Pasted text +%d lines]", pasteChipMinLines+4)
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("DisplayValue = %q, want %q", got, wantLabel)
	}
	if got := p.Value(); got != big {
		t.Fatalf("Value should expand chip back to original content")
	}
}

// TestBackspaceAtChipEndRemovesWholeChip: cursor at the rune right after the
// chip label, Backspace once, the whole label disappears and any
// surrounding text stays intact.
func TestBackspaceAtChipEndRemovesWholeChip(t *testing.T) {
	p := newChippablePrompt()
	// Type a prefix, paste big, type a suffix.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("before ")})
	p, _ = p.Update(pasteKey(makePaste(10)))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" after")})

	// Move cursor to the very end of the chip label (before " after").
	// Chip span starts at 7 ("before ") and is 23 runes long ("[Pasted text +10 lines]" = 23).
	// Position cursor at span.end via Home + right-arrows.
	p.setCursorRuneOffset(p.spans[0].end)

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(p.spans) != 0 {
		t.Fatalf("Backspace at chip.end must remove the chip; spans=%+v", p.spans)
	}
	if got := p.DisplayValue(); got != "before  after" {
		t.Fatalf("after delete value = %q, want 'before  after'", got)
	}
}

// TestDeleteAtChipStartRemovesWholeChip: cursor at the rune right before the
// chip label, forward-Delete removes the whole chip.
func TestDeleteAtChipStartRemovesWholeChip(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("prefix")})
	p, _ = p.Update(pasteKey(makePaste(12)))

	// Cursor at chip start.
	p.setCursorRuneOffset(p.spans[0].start)

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if len(p.spans) != 0 {
		t.Fatalf("Delete at chip.start must remove the chip; spans=%+v", p.spans)
	}
	if got := p.DisplayValue(); got != "prefix" {
		t.Fatalf("after delete value = %q, want 'prefix'", got)
	}
}

// TestLeftArrowAtChipEndJumpsToStart: ← at chip.end moves the cursor to
// chip.start in one keystroke — cursor never visits interior positions.
func TestLeftArrowAtChipEndJumpsToStart(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(9)))
	p.setCursorRuneOffset(p.spans[0].end)

	before := p.cursorRuneOffset()
	wantStart := p.spans[0].start
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyLeft})
	after := p.cursorRuneOffset()

	if before == after {
		t.Fatal("← should have moved the cursor")
	}
	if after != wantStart {
		t.Fatalf("← at chip.end should land at chip.start=%d, got %d", wantStart, after)
	}
	if len(p.spans) != 1 {
		t.Fatal("← must not delete the chip")
	}
}

// TestRightArrowAtChipStartJumpsToEnd: → at chip.start skips over the whole
// chip in one keystroke.
func TestRightArrowAtChipStartJumpsToEnd(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(9)))
	p.setCursorRuneOffset(p.spans[0].start)

	wantEnd := p.spans[0].end
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRight})
	after := p.cursorRuneOffset()

	if after != wantEnd {
		t.Fatalf("→ at chip.start should land at chip.end=%d, got %d", wantEnd, after)
	}
	if len(p.spans) != 1 {
		t.Fatal("→ must not delete the chip")
	}
}

// TestTwoChipsTrackedIndependently: two separate big pastes produce two
// chips. Deleting the first leaves the second intact and its span shifts
// left by the first's label length.
func TestTwoChipsTrackedIndependently(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(8)))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" mid ")})
	p, _ = p.Update(pasteKey(makePaste(15)))

	if len(p.spans) != 2 {
		t.Fatalf("expected 2 chips, got %d", len(p.spans))
	}
	if p.spans[0].start >= p.spans[1].start {
		t.Fatal("chip spans must be sorted left-to-right")
	}

	// Delete the first chip via Backspace at its end.
	p.setCursorRuneOffset(p.spans[0].end)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if len(p.spans) != 1 {
		t.Fatalf("after deleting first chip, one should remain; got %d", len(p.spans))
	}
	// The remaining chip is the one with 15 lines.
	content, ok := p.store[p.spans[0].id]
	if !ok {
		t.Fatal("remaining span has no store entry")
	}
	if content.lines != 15 {
		t.Fatalf("remaining chip should be the 15-line one, got %d", content.lines)
	}
}

// TestValueExpandsAllChips: multi-chip Value() concatenates surrounding text
// with full paste contents in the right order.
func TestValueExpandsAllChips(t *testing.T) {
	p := newChippablePrompt()
	paste1 := makePaste(9)
	paste2 := makePaste(11)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A ")})
	p, _ = p.Update(pasteKey(paste1))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" B ")})
	p, _ = p.Update(pasteKey(paste2))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" C")})

	want := "A " + paste1 + " B " + paste2 + " C"
	if got := p.Value(); got != want {
		t.Fatalf("Value expansion wrong:\nwant %q\ngot  %q", want, got)
	}
}

// TestTypingShiftsSpans: inserting a character before a chip shifts the
// chip's span right by one; reconcile picks this up automatically.
func TestTypingShiftsSpans(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	origStart := p.spans[0].start
	if origStart != 0 {
		t.Fatalf("precondition: chip starts at 0, got %d", origStart)
	}

	// Move cursor home, type a character.
	p.setCursorRuneOffset(0)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})

	if len(p.spans) != 1 {
		t.Fatalf("typing before chip must not destroy it, spans=%+v", p.spans)
	}
	if p.spans[0].start != 1 {
		t.Fatalf("chip start should shift to 1, got %d", p.spans[0].start)
	}
}

// TestEntryRestoreRoundTrip: Entry() + Restore() fully recovers chip state —
// same display text, same chip spans, same expanded Value(). Prompt history
// uses this for ↑/↓ replay.
func TestEntryRestoreRoundTrip(t *testing.T) {
	p := newChippablePrompt()
	paste := makePaste(12)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pre ")})
	p, _ = p.Update(pasteKey(paste))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" post")})

	entry := p.Entry()
	wantDisplay := p.DisplayValue()
	wantValue := p.Value()

	// New prompt, restore.
	q := newChippablePrompt()
	q.Restore(entry)

	if got := q.DisplayValue(); got != wantDisplay {
		t.Fatalf("DisplayValue mismatch after Restore: %q vs %q", got, wantDisplay)
	}
	if got := q.Value(); got != wantValue {
		t.Fatalf("Value mismatch after Restore")
	}
	if len(q.spans) != 1 {
		t.Fatalf("expected 1 span after Restore, got %d", len(q.spans))
	}
}

// TestResetClearsChips: after Reset the store and spans are empty so
// subsequent Value()/DisplayValue() return just the new text.
func TestResetClearsChips(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	if len(p.spans) != 1 {
		t.Fatal("precondition: one chip")
	}
	p.Reset()
	if p.DisplayValue() != "" {
		t.Fatalf("Reset should empty the textarea, got %q", p.DisplayValue())
	}
	if len(p.spans) != 0 || len(p.store) != 0 {
		t.Fatal("Reset should clear chip state")
	}
}

// TestSetValueClearsChips: SetValue replaces with plain text and forgets any
// prior chip. Used by slash-popover Tab-completion where chips can never be
// part of the replacement.
func TestSetValueClearsChips(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	p.SetValue("/models work")
	if p.DisplayValue() != "/models work" {
		t.Fatalf("SetValue should install text verbatim, got %q", p.DisplayValue())
	}
	if len(p.spans) != 0 {
		t.Fatal("SetValue should drop any existing chips")
	}
}

// TestBackspaceNotAtChipBoundaryFallsThrough: Backspace away from any chip
// boundary deletes one character normally and leaves the chip alone.
func TestBackspaceNotAtChipBoundaryFallsThrough(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	p, _ = p.Update(pasteKey(makePaste(9)))
	// Cursor ends up at the very end, past the chip.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	// Now cursor is at end; backspace should remove the "X", NOT the chip.

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(p.spans) != 1 {
		t.Fatal("Backspace after non-chip character must not remove the chip")
	}
	if strings.HasSuffix(p.DisplayValue(), "X") {
		t.Fatal("Backspace should have removed the trailing 'X'")
	}
}

// TestPageKeysMoveCursorByHeight: PgUp / PgDn in a prompt large enough to
// scroll internally should move the cursor by one prompt-height of lines —
// bubbles/textarea's viewport keymap is empty, so without this the keys
// are no-ops while mouse wheel (via the textarea's MouseMsg path) already
// scrolls.
func TestPageKeysMoveCursorByHeight(t *testing.T) {
	p := newChippablePrompt()
	// Fill the textarea with many logical rows. SetValue takes the whole
	// value at once — cheaper than typing + Enter per line and avoids the
	// chip-paste threshold entirely.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "row%02d\n", i)
	}
	p.SetValue(b.String())
	p.CursorEnd()
	startRow := p.ta.Line()
	if startRow < 10 {
		t.Fatalf("precondition: cursor should be deep into the content, got row %d", startRow)
	}

	// PgUp should move the cursor up — the textarea's repositionView will
	// scroll its internal viewport along with it.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if p.ta.Line() >= startRow {
		t.Fatalf("PgUp should move cursor up from row %d, ended at %d",
			startRow, p.ta.Line())
	}

	upRow := p.ta.Line()
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if p.ta.Line() <= upRow {
		t.Fatalf("PgDn should move cursor down from row %d, ended at %d",
			upRow, p.ta.Line())
	}
}

// TestCarriageReturnLineEndings: terminals that send \r as a line separator
// (old-mac-style, some VS Code setups under certain TERM settings) must
// still get a correct line count in the chip label. bubbles/textarea only
// splits on \n; we have to count line separators ourselves.
func TestCarriageReturnLineEndings(t *testing.T) {
	p := newChippablePrompt()
	// 10-line paste with \r separators only.
	paste := strings.Repeat("line\r", 9) + "end"
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste), Paste: true})

	if len(p.spans) != 1 {
		t.Fatalf("\\r-separated paste should still chip; spans=%+v", p.spans)
	}
	wantLabel := "[Pasted text +10 lines]"
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("label should report the real line count; got %q want %q",
			got, wantLabel)
	}
}

// TestCRLFLineEndings: Windows-style \r\n separators count each logical
// line once, not twice.
func TestCRLFLineEndings(t *testing.T) {
	p := newChippablePrompt()
	paste := strings.Repeat("line\r\n", 9) + "end"
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste), Paste: true})

	if len(p.spans) != 1 {
		t.Fatal("CRLF paste should chip")
	}
	wantLabel := "[Pasted text +10 lines]"
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("label line count wrong for CRLF; got %q want %q",
			got, wantLabel)
	}
}

// TestPasteWithoutFlagButWithNewlineStillChips: some terminals don't set
// the bracketed-paste flag even though bubbletea's rune collector delivers
// multi-line content in one message (impossible for regular typing — the
// collector breaks on \n). We accept those as pastes too so the feature
// isn't silently dead on less-common terminals.
func TestPasteWithoutFlagButWithNewlineStillChips(t *testing.T) {
	p := newChippablePrompt()
	msg := tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(makePaste(10)),
		Paste: false, // <- key point: no flag
	}
	p, _ = p.Update(msg)
	if len(p.spans) != 1 {
		t.Fatalf("paste-like KeyMsg without Paste flag should still chip; spans=%+v", p.spans)
	}
}

// TestLongSingleLinePasteChipsByCharCount: a 500-character single-line blob
// has zero newlines but still deserves to collapse — line-count alone would
// miss minified JSON and long stack-trace-on-one-line cases.
func TestLongSingleLinePasteChipsByCharCount(t *testing.T) {
	p := newChippablePrompt()
	big := strings.Repeat("x", 500)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(big), Paste: true})
	if len(p.spans) != 1 {
		t.Fatalf("long single-line paste should chip by char threshold; spans=%+v", p.spans)
	}
	wantLabel := "[Pasted text +1 lines]"
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("DisplayValue = %q, want %q", got, wantLabel)
	}
}

// TestBackspaceImmediatelyAfterChipRemovesIt: regression guard for the
// atomic semantics — user pastes, cursor lands right after chip, single
// Backspace deletes the whole thing without needing to set cursor manually.
func TestBackspaceImmediatelyAfterChipRemovesIt(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	// InsertString leaves the cursor at the end of the inserted content —
	// so cursor is exactly at chip.end. Backspace should trip the atomic
	// path directly.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if len(p.spans) != 0 {
		t.Fatalf("single Backspace right after paste should remove chip, spans=%+v", p.spans)
	}
	if p.DisplayValue() != "" {
		t.Fatalf("prompt should be empty after chip delete, got %q", p.DisplayValue())
	}
}
