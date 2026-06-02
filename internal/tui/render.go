package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// splashCode and splashHamr form the two-tone "CODEHAMR" wordmark printed
// once at startup, pushed into scrollback via tea.Println; it scrolls up
// naturally as content arrives, so View() needs no hide-on-first-content branch.
var splashCode = []string{
	" ██████  ██████  ██████   ███████ ",
	"██      ██    ██ ██   ██  ██      ",
	"██      ██    ██ ██   ██  █████   ",
	"██      ██    ██ ██   ██  ██      ",
	" ██████  ██████  ██████   ███████ ",
}

var splashHamr = []string{
	"██   ██  █████  ███    ███ ██████  ",
	"██   ██ ██   ██ ████  ████ ██   ██ ",
	"███████ ███████ ██ ████ ██ ██████  ",
	"██   ██ ██   ██ ██  ██  ██ ██   ██ ",
	"██   ██ ██   ██ ██      ██ ██   ██ ",
}

// appendLine queues a styled line for tea.Println on the next Update cycle,
// so the terminal, not us, owns the scrollback. scroll is a passive
// write-only transcript: never rendered, read only by tests and the debug log.
func (m *Model) appendLine(s string) {
	m.scroll.WriteString(s + "\n")
	m.outbox = append(m.outbox, s)
}

// flushStreaming ends the content phase: render the streaming buffer through
// glamour, queue it for tea.Println, reset. No-op on an empty buffer. Glamour
// errors fall back to raw so partial docs (unclosed code fence on cancel) survive.
func (m *Model) flushStreaming() {
	if m.streaming.Len() == 0 {
		return
	}
	raw := m.streaming.String()
	rendered, err := m.renderer.Render(raw)
	if err != nil {
		rendered = raw
	}
	// Strip glamour's trailing newline so tea.Println doesn't double-space
	// the next prompt below the block.
	m.appendLine(strings.TrimRight(rendered, "\n"))
	m.streaming.Reset()
}

// chromeHeight is the non-resizable vertical chrome (separator + status bar)
// recomputeLayout subtracts when capping the textarea against the window.
const chromeHeight = 2

// recomputeLayout caps the textarea height so a long paste can't push the
// status bar off-screen: leave minViewport rows of breathing room above it.
// Cheap enough to run on every key press.
func (m *Model) recomputeLayout() {
	m.ta.SetHeight(max(1, min(m.visualPromptLines(), m.maxTextareaHeight())))
}

// maxTextareaHeight caps the textarea: terminal minus chrome, breathing room,
// and any active popover. Shared by recomputeLayout and preGrowTextarea.
func (m *Model) maxTextareaHeight() int {
	if m.height <= 0 {
		return 1
	}
	return max(1, m.height-minViewport-chromeHeight-m.popoverHeight())
}

// preGrowTextarea inflates Height to the cap *before* a KeyMsg is processed.
// bubbles/textarea's repositionView() scrolls the viewport down when the
// cursor drops below Height (e.g. a typed char wraps); since recomputeLayout
// runs only AFTER handleKey, without this the viewport stays anchored at the
// scrolled YOffset and hides the earliest wrap rows. Pre-growing keeps the
// cursor in view so no scroll fires; recomputeLayout then shrinks back,
// leaving YOffset at 0 and every wrapped row visible from the top.
func (m *Model) preGrowTextarea() {
	cap := m.maxTextareaHeight()
	if cap > m.ta.Height() {
		m.ta.SetHeight(cap)
	}
}

// visualPromptLines counts the *visual* rows the textarea needs (a line that
// wraps to three screen rows wants a three-row textarea), via wrapRows which
// mirrors bubbles/textarea's wrap() (see there for the grapheme-cluster
// caveat). Reads DisplayValue so a chip counts as one line, not the hundreds
// its expanded content would.
func (m *Model) visualPromptLines() int {
	w := m.width - 4 // -2 for ta.SetWidth offset, -2 for "▌ " prompt
	if w < 1 {
		return 1
	}
	total := 0
	for line := range strings.SplitSeq(m.ta.DisplayValue(), "\n") {
		total += wrapRows(line, w)
	}
	if total < 1 {
		return 1
	}
	return total
}

// wrapRows mirrors bubbles/textarea.wrap()'s row count so the prompt's
// auto-grow stays in lock step with what the textarea renders: word-boundary
// aware with a hard-wrap fallback for over-wide words, plus the trailing
// cursor-anchor row when content exactly fills the width.
// Adapted from charmbracelet/bubbles v0.20 textarea.
//
// Caveat: width is summed per-rune (runewidth) rather than per grapheme cluster
// like bubbles' uniseg, so ASCII and CJK match exactly, but a multi-rune cluster
// (a ZWJ-family emoji, a keycap) can over-count, harmlessly over-growing the
// prompt by a row on emoji-heavy input. Not worth pulling in uniseg for that.
func wrapRows(s string, width int) int {
	if width <= 0 {
		return 1
	}
	row := 0
	var lineW, wordW, spaces, charW int
	var hadWord bool

	for _, r := range s {
		charW = 0
		if unicode.IsSpace(r) {
			spaces++
		} else {
			charW = runewidth.RuneWidth(r)
			wordW += charW
			hadWord = true
		}

		switch {
		case spaces > 0:
			if lineW+wordW+spaces > width {
				row++
				lineW = wordW + spaces
			} else {
				lineW += wordW + spaces
			}
			wordW, spaces = 0, 0
			hadWord = false
		case hadWord && wordW+charW > width:
			// Space-less word grew past the width; matches bubbles'
			// StringWidth(word)+lastCharLen check.
			if lineW > 0 {
				row++
			}
			lineW = wordW
			wordW = 0
			hadWord = false
		}
	}

	if lineW+wordW+spaces >= width {
		row++
	}
	return row + 1
}

// View renders only the live bottom region: in-flight streaming tokens, the
// popover, a divider, the prompt, and the status bar. Everything else has
// already gone to scrollback via tea.Println, scrolled with the terminal's
// own wheel/PgUp like any shell session.
func (m Model) View() string {
	if m.width == 0 || m.suppressView {
		// No WindowSizeMsg yet, or a width-resize mid-drag: an empty frame
		// is safest. A 0-wide layout flashes garbled, and a real frame
		// mid-drag races the renderer's stale-flush window.
		return ""
	}
	var pieces []string
	if m.streaming.Len() > 0 {
		pieces = append(pieces, ansi.Wrap(m.streaming.String(), m.width, ""))
	}
	if p := m.renderPopover(); p != "" {
		pieces = append(pieces, p)
	}
	// Divider one cell narrower than m.width, and pieces joined with bare
	// "\n" (not lipgloss.JoinVertical's Left-pad): a line ending in the last
	// column trips Apple Terminal.app's last-column-wrap (DECAWM xn)
	// inconsistently, drifting bubbletea's inline line count by one per frame:
	// a duplicated prompt line overwrites the status bar on macOS (other
	// terminals stay clean). Keeping the last column blank sidesteps it.
	pieces = append(pieces,
		styleDim.Render(strings.Repeat("─", max(m.width-1, 1))),
		m.ta.View(),
		m.renderStatusBar(),
	)
	return strings.Join(pieces, "\n")
}

// splashLines builds the identity block for tea.Println. Below wordmarkWidth
// the ASCII art soft-wraps into garbage, so collapse to plain text.
func (m Model) splashLines() []string {
	const wordmarkWidth = 70 // cells needed for CODE+HAMR side-by-side
	if m.width >= wordmarkWidth {
		lines := []string{""}
		for i := range splashCode {
			lines = append(lines, "  "+styleDim.Render(splashCode[i])+styleHamr.Render(splashHamr[i]))
		}
		lines = append(lines,
			"", styleDim.Render("  It's hamr time!"),
			"", styleDim.Render(fmt.Sprintf("  codehamr %s · %s @ %s",
				m.Version, m.cfg.ActiveProfile().LLM, m.cfg.Active)),
			"",
			styleDim.Render("  AI systems can make mistakes. Codehamr executes their commands with full shell and filesystem access."),
			styleDim.Render("  Run inside a devcontainer or VM where it cannot cause damage outside the sandbox."),
			"",
		)
		return lines
	}
	return []string{
		"",
		styleHamr.Render("  codehamr"),
		styleDim.Render(fmt.Sprintf("  %s · %s @ %s",
			m.Version, m.cfg.ActiveProfile().LLM, m.cfg.Active)),
		"",
		styleDim.Render("  Sandboxed AI shell - run in a devcontainer or VM."),
		"",
	}
}

func (m Model) renderStatusBar() string {
	sep := styleStatus.Render(" · ")
	segs := []string{backendLabel(m.cfg, m.connected)}

	if live := m.sessionTokens + m.streamingEstimate; live > 0 {
		segs = appendStatus(segs, humanTokens(live))
	}
	if suf := m.budget.StatusSuffix(); suf != "" {
		segs = appendStatus(segs, strings.TrimPrefix(suf, " · "))
	}
	if label := m.phase.label(); label != "" {
		segs = appendStatus(segs, m.spinner.View()+" "+label)
		segs = appendStatus(segs, liveElapsed(time.Since(m.turnStart)))
	} else if mark := m.lastOutcome.marker(); mark != "" {
		// Frozen run summary at idle, until the next submit: outcome glyph,
		// wall-clock duration, and the avg rate that divides into it.
		seg := mark + " " + liveElapsed(m.lastElapsed)
		if avg := humanRate(m.lastTokens, m.lastElapsed); avg != "" {
			seg += " · " + avg + " avg"
		}
		segs = appendStatus(segs, seg)
	}
	if m.status != "" {
		segs = appendStatus(segs, m.status)
	}
	return strings.Join(segs, sep)
}

// appendStatus wraps one segment in styleStatus and appends it, keeping
// renderStatusBar readable instead of repeating the wrap at each call site.
func appendStatus(segs []string, s string) []string {
	return append(segs, styleStatus.Render(s))
}
