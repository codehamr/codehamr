package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pasteChipMinLines is the line-count threshold above which a bracketed-paste
// event collapses into an atomic chip. Below this, short multi-line pastes
// (a handful of code lines, a few sentences) stay inline and readable.
//
// pasteChipMinChars is the character-count fallback for long single-line
// pastes — minified JSON, a full URL-encoded blob, a log line of a thousand
// characters. Line-count alone would miss those.
const (
	pasteChipMinLines = 5
	pasteChipMinChars = 200
)

// promptInput wraps bubbles/textarea with an atomic-chip model. Clipboard
// pastes of ≥pasteChipMinLines lines collapse into a single inline label
// [Pasted text +N lines] that behaves as one character for cursor moves and
// deletion. The original paste content is kept in store, keyed by id, and
// expanded back when Value() is read for LLM submission.
type promptInput struct {
	ta     textarea.Model
	store  map[int]chipContent
	spans  []chipSpan
	nextID int
}

// chipContent is the payload behind a chip — kept in promptInput.store keyed
// by id so the visible label can collapse while the real text survives for
// Value() and history replay.
type chipContent struct {
	content string
	lines   int
}

// chipSpan marks the rune range [start, end) in the textarea's value where a
// chip's label currently lives. Reconciled after every key event that can
// shift the value.
type chipSpan struct {
	id         int
	start, end int
}

// promptEntry is a frozen snapshot of a promptInput for history replay —
// contains the exact text as displayed plus enough chip metadata to restore
// the atomic-chip behaviour on ↑/↓ recall.
type promptEntry struct {
	display string
	store   map[int]chipContent
	spans   []chipSpan
}

// newPromptInput builds a configured textarea and the surrounding chip
// bookkeeping. All the styling decisions (bare base, accent prompt, accent
// cursor) live here so model.go never touches textarea internals directly.
func newPromptInput() promptInput {
	ta := textarea.New()
	ta.Placeholder = "Ask codehamr. / or Tab for commands · Ctrl+C cancels"
	ta.Focus()
	ta.CharLimit = 0
	ta.MaxHeight = 0 // 0 = unbounded; recomputeLayout enforces the cap
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetWidth(defaultWidth - 2)
	ta.Prompt = "▌ "

	bare := lipgloss.NewStyle()
	ta.FocusedStyle.Base = bare
	ta.FocusedStyle.CursorLine = bare
	ta.FocusedStyle.CursorLineNumber = bare
	ta.FocusedStyle.EndOfBuffer = bare
	ta.FocusedStyle.LineNumber = bare
	ta.FocusedStyle.Placeholder = styleDim
	ta.FocusedStyle.Prompt = stylePrompt
	ta.FocusedStyle.Text = bare
	ta.BlurredStyle = ta.FocusedStyle
	ta.Cursor.Style = styleHamr

	return promptInput{
		ta:    ta,
		store: map[int]chipContent{},
	}
}

// chipLabel is the human-visible form inserted into the textarea. Plain text,
// no ANSI — bubbles/textarea doesn't render inline styling inside its value.
func chipLabel(lines int) string {
	return fmt.Sprintf("[Pasted text +%d lines]", lines)
}

// Update is the promptInput's message entry point. Bracketed-paste events of
// sufficient size are swallowed here and converted into a chip; chip-aware
// key handling happens before delegation; everything else falls through to
// the wrapped textarea. reconcile() runs after any path that could have
// shifted the value.
func (p promptInput) Update(msg tea.Msg) (promptInput, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyMsg); ok {
		if looksLikePaste(kmsg) && shouldChip(string(kmsg.Runes)) {
			p.insertChip(string(kmsg.Runes))
			return p, nil
		}
		if handled, next := p.handlePageKey(kmsg); handled {
			return next, nil
		}
		if handled, next := p.handleChipKey(kmsg); handled {
			return next, nil
		}
	}
	var cmd tea.Cmd
	p.ta, cmd = p.ta.Update(msg)
	p.reconcile()
	return p, cmd
}

// handlePageKey implements PgUp / PgDn for the prompt field. bubbles/
// textarea disables its internal viewport's keymap, so page keys aren't
// wired by default — we translate them into N×CursorUp / N×CursorDown,
// which moves the cursor one visible page and lets textarea.repositionView
// scroll the viewport to match. If the cursor lands strictly inside a chip
// span after the move, snap it to the nearest boundary so the cursor
// doesn't render mid-label.
func (p promptInput) handlePageKey(msg tea.KeyMsg) (bool, promptInput) {
	var step func()
	switch msg.Type {
	case tea.KeyPgUp:
		step = func() { p.ta.CursorUp() }
	case tea.KeyPgDown:
		step = func() { p.ta.CursorDown() }
	default:
		return false, p
	}
	n := max(p.ta.Height(), 1)
	for range n {
		step()
	}
	p.snapCursorOutOfChip()
	return true, p
}

// snapCursorOutOfChip ensures the cursor never stays strictly inside a chip
// span, snapping to the nearer boundary when it does. Used after bulk
// navigation (PgUp/PgDn) and at the entry to handleChipKey. Returns the
// (possibly adjusted) cursor offset so callers can act on it without a
// second cursorRuneOffset read.
func (p *promptInput) snapCursorOutOfChip() int {
	cur := p.cursorRuneOffset()
	for _, chip := range p.spans {
		if cur > chip.start && cur < chip.end {
			target := chip.end
			if cur-chip.start < chip.end-cur {
				target = chip.start
			}
			p.setCursorRuneOffset(target)
			return target
		}
	}
	return cur
}

// chipAtBoundary returns the chip whose start (atStart=true) or end
// (atStart=false) coincides with cur. Encapsulates the "cursor sits on a
// chip boundary" check that Backspace/Delete/Left/Right all need.
func (p promptInput) chipAtBoundary(cur int, atStart bool) (chipSpan, bool) {
	for _, chip := range p.spans {
		boundary := chip.end
		if atStart {
			boundary = chip.start
		}
		if cur == boundary {
			return chip, true
		}
	}
	return chipSpan{}, false
}

// looksLikePaste recognises paste-like key events. The primary signal is the
// bracketed-paste Paste flag bubbletea sets when the terminal wraps the
// content in \x1b[200~...\x1b[201~. Some terminals don't emit those markers
// though — as a fallback we also treat any KeyRunes event whose Runes
// contain a newline as a paste, since bubbletea's rune collector breaks on
// control characters, so a regular keystroke can never produce a newline
// inside a single KeyMsg.
func looksLikePaste(msg tea.KeyMsg) bool {
	if msg.Paste {
		return true
	}
	if msg.Type != tea.KeyRunes {
		return false
	}
	for _, r := range msg.Runes {
		if r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}

// shouldChip decides whether a paste is large enough to collapse. Either the
// line count or the character count must clear its threshold — lines catch
// the usual multi-line paste, chars catch long single-line blobs like
// minified JSON or a stack trace on one line.
func shouldChip(s string) bool {
	if countLines(s) >= pasteChipMinLines {
		return true
	}
	return utf8.RuneCountInString(s) >= pasteChipMinChars
}

// countLines returns the visual line count of a paste. Terminals disagree on
// line separators — unix sends \n, old-mac style sends \r, Windows sends
// \r\n. Taking the max of the \n and \r counts handles all three without
// double-counting \r\n (which still reports the correct number of \n).
func countLines(s string) int {
	n := strings.Count(s, "\n")
	if r := strings.Count(s, "\r"); r > n {
		n = r
	}
	return n + 1
}

// handleChipKey implements the atomic-token semantics: Backspace/Delete at a
// chip boundary removes the whole chip; ←/→ at a chip boundary jumps across;
// a cursor that somehow lands strictly inside a chip gets snapped to the
// nearest boundary before any other key runs. Returns (handled, updated).
func (p promptInput) handleChipKey(msg tea.KeyMsg) (bool, promptInput) {
	if len(p.spans) == 0 {
		return false, p
	}
	cur := p.snapCursorOutOfChip()
	switch msg.Type {
	case tea.KeyBackspace:
		if chip, ok := p.chipAtBoundary(cur, false); ok {
			p.deleteSpan(chip)
			return true, p
		}
	case tea.KeyDelete:
		if chip, ok := p.chipAtBoundary(cur, true); ok {
			p.deleteSpan(chip)
			return true, p
		}
	case tea.KeyLeft:
		if chip, ok := p.chipAtBoundary(cur, false); ok {
			p.setCursorRuneOffset(chip.start)
			return true, p
		}
	case tea.KeyRight:
		if chip, ok := p.chipAtBoundary(cur, true); ok {
			p.setCursorRuneOffset(chip.end)
			return true, p
		}
	}
	return false, p
}

// insertChip splices a new chip label into the textarea at the cursor. The
// spans slice stays sorted by start position — find the correct insertion
// index, shift subsequent spans by the label length, and splice in the new
// one. reconcile() at the end is a belt-and-braces check; on the happy path
// it doesn't change anything.
func (p *promptInput) insertChip(content string) {
	lines := countLines(content)
	id := p.nextID
	p.nextID++
	p.store[id] = chipContent{content: content, lines: lines}

	label := chipLabel(lines)
	labelLen := utf8.RuneCountInString(label)
	insertAt := p.cursorRuneOffset()

	insertIdx := 0
	for i, s := range p.spans {
		if s.start < insertAt {
			insertIdx = i + 1
		} else {
			break
		}
	}
	for i := insertIdx; i < len(p.spans); i++ {
		p.spans[i].start += labelLen
		p.spans[i].end += labelLen
	}
	p.spans = slices.Insert(p.spans, insertIdx,
		chipSpan{id: id, start: insertAt, end: insertAt + labelLen})

	p.ta.InsertString(label)
	p.reconcile()
}

// deleteSpan removes the chip's label from the textarea value and drops the
// chip from both spans and store. Cursor lands at the now-vacated span start.
// Subsequent spans are re-validated via reconcile — they'll shift left by
// the removed label length.
func (p *promptInput) deleteSpan(chip chipSpan) {
	value := p.ta.Value()
	runes := []rune(value)
	if chip.end > len(runes) {
		return
	}
	spliced := string(runes[:chip.start]) + string(runes[chip.end:])
	p.ta.SetValue(spliced)
	p.setCursorRuneOffset(chip.start)
	delete(p.store, chip.id)
	p.spans = slices.DeleteFunc(p.spans, func(s chipSpan) bool { return s.id == chip.id })
	p.reconcile()
}

// reconcile walks the current spans in order, searches for each chip's label
// in the textarea value starting after the previous span's end, and updates
// offsets. A span whose label has vanished (e.g. partially deleted by some
// non-chip-aware edit) is dropped along with its store entry — the chip
// effectively becomes plain text from that moment on.
func (p *promptInput) reconcile() {
	value := p.ta.Value()
	valueRunes := []rune(value)
	kept := make([]chipSpan, 0, len(p.spans))
	searchFrom := 0
	for _, span := range p.spans {
		content, ok := p.store[span.id]
		if !ok {
			continue
		}
		label := chipLabel(content.lines)
		labelRunes := []rune(label)
		idx := runeIndex(valueRunes[searchFrom:], labelRunes)
		if idx < 0 {
			delete(p.store, span.id)
			continue
		}
		start := searchFrom + idx
		end := start + len(labelRunes)
		kept = append(kept, chipSpan{id: span.id, start: start, end: end})
		searchFrom = end
	}
	p.spans = kept
}

// runeIndex is a rune-level strings.Index: find the first occurrence of
// needle in haystack, both as []rune, return rune offset or -1. We work in
// runes throughout promptInput because bubbles/textarea's cursor is rune-
// addressed (column = rune count, not byte count).
func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// cursorRuneOffset returns the cursor position as an absolute rune index into
// the flat Value() string. bubbles/textarea only exposes (row, col) directly
// plus LineInfo().{StartColumn, ColumnOffset} — enough to reconstruct the
// absolute position by walking prior lines and their rune counts. Uses
// SplitSeq so the prior-lines walk doesn't materialise a slice on every
// chip-aware keypress.
func (p promptInput) cursorRuneOffset() int {
	row := p.ta.Line()
	info := p.ta.LineInfo()
	col := info.StartColumn + info.ColumnOffset

	offset, i := 0, 0
	for line := range strings.SplitSeq(p.ta.Value(), "\n") {
		if i == row {
			n := utf8.RuneCountInString(line)
			if col > n {
				col = n
			}
			return offset + col
		}
		offset += utf8.RuneCountInString(line) + 1 // +1 for the \n
		i++
	}
	return offset
}

// setCursorRuneOffset moves the cursor to the given absolute rune position.
// Navigates by CursorUp/CursorDown to reach the target row, then SetCursor
// to land at the target column. No direct (row, col) setter exists on the
// textarea, so this stepwise approach is what we've got. The guard caps
// total walk steps so a buggy textarea can never wedge the loop.
func (p *promptInput) setCursorRuneOffset(offset int) {
	value := p.ta.Value()
	targetRow, targetCol := runeOffsetToRowCol(value, offset)

	for range 2048 {
		curRow := p.ta.Line()
		if curRow == targetRow {
			break
		}
		if curRow < targetRow {
			p.ta.CursorDown()
		} else {
			p.ta.CursorUp()
		}
		if p.ta.Line() == curRow {
			break // step did nothing, bail rather than spin
		}
	}
	p.ta.SetCursor(targetCol)
}

// runeOffsetToRowCol converts an absolute rune offset into (row, col) for
// positional API calls. Cheap enough to recompute on demand.
func runeOffsetToRowCol(value string, offset int) (int, int) {
	row, col := 0, 0
	i := 0
	for _, r := range value {
		if i == offset {
			return row, col
		}
		if r == '\n' {
			row++
			col = 0
		} else {
			col++
		}
		i++
	}
	return row, col
}

// View delegates straight to the underlying textarea. Chip labels are already
// plain text in the value, so no post-processing is needed for v1.
func (p promptInput) View() string { return p.ta.View() }

// Value returns the prompt text with every chip label expanded to its
// original content. This is what goes to the LLM on submit.
func (p promptInput) Value() string {
	if len(p.spans) == 0 {
		return p.ta.Value()
	}
	value := p.ta.Value()
	runes := []rune(value)
	var b strings.Builder
	b.Grow(len(value))
	cursor := 0
	for _, span := range p.spans {
		content, ok := p.store[span.id]
		if !ok {
			continue
		}
		if span.start > cursor {
			b.WriteString(string(runes[cursor:span.start]))
		}
		b.WriteString(content.content)
		cursor = span.end
	}
	if cursor < len(runes) {
		b.WriteString(string(runes[cursor:]))
	}
	return b.String()
}

// DisplayValue returns the exact text shown in the textarea — chip labels
// stay collapsed. Used for echo-to-scroll on submit and for the ↑/↓ history
// snapshot.
func (p promptInput) DisplayValue() string { return p.ta.Value() }

// Entry snapshots the current state for the history buffer. We clone the
// store and spans so later edits to the live promptInput don't mutate the
// recorded entry.
func (p promptInput) Entry() promptEntry {
	return promptEntry{
		display: p.ta.Value(),
		store:   maps.Clone(p.store),
		spans:   slices.Clone(p.spans),
	}
}

// Restore replays a history entry into the live promptInput. Clears existing
// chips, sets the display text, installs the snapshot's chip state, drops
// the cursor at the end.
func (p *promptInput) Restore(entry promptEntry) {
	p.ta.SetValue(entry.display)
	p.store = maps.Clone(entry.store)
	if p.store == nil {
		p.store = map[int]chipContent{}
	}
	p.spans = slices.Clone(entry.spans)
	p.ta.CursorEnd()
}

// Reset clears the typed text and all chip state. nextID is preserved so ids
// stay monotonic within a session — makes debugging easier.
func (p *promptInput) Reset() {
	p.ta.Reset()
	p.store = map[int]chipContent{}
	p.spans = nil
}

// SetValue installs a plain-text value, dropping any chip state. Used by the
// slash popover's Tab-completion path where no chip can ever be injected.
func (p *promptInput) SetValue(s string) {
	p.ta.SetValue(s)
	p.store = map[int]chipContent{}
	p.spans = nil
}

func (p *promptInput) SetWidth(w int)  { p.ta.SetWidth(w) }
func (p *promptInput) SetHeight(h int) { p.ta.SetHeight(h) }
func (p promptInput) Height() int      { return p.ta.Height() }
func (p promptInput) Line() int        { return p.ta.Line() }
func (p promptInput) LineCount() int   { return p.ta.LineCount() }
func (p *promptInput) CursorEnd()      { p.ta.CursorEnd() }
