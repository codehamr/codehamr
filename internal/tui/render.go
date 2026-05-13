package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// splashCode and splashHamr together form the two-tone "CODEHAMR" wordmark
// printed once at startup. With native terminal scrolling we just push it
// into scrollback via tea.Println; it scrolls up naturally as content
// arrives, so we no longer maintain a "splash hides on first content"
// branch in View().
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

// appendLine queues a single styled line for emission to the terminal
// scrollback on the next Update cycle. The line is flushed via tea.Println
// in the Update wrapper, so the terminal — not us — owns the scrollback.
// scroll is a passive write-only transcript: never rendered, used only by
// tests and the optional debug log to verify what was emitted.
func (m *Model) appendLine(s string) {
	m.scroll.WriteString(s + "\n")
	m.outbox = append(m.outbox, s)
}

// flushStreaming ends the current content phase: render the raw streaming
// buffer through glamour, queue the rendered block for tea.Println, reset
// the streaming buffer. Safe to call repeatedly — a zero-length streaming
// buffer is a no-op. Glamour errors fall back to raw content so partial
// documents (unclosed code fences on cancel/error) still survive.
func (m *Model) flushStreaming() {
	if m.streaming.Len() == 0 {
		return
	}
	raw := m.streaming.String()
	rendered, err := m.renderer.Render(raw)
	if err != nil {
		rendered = raw
	}
	// glamour adds a trailing newline; strip once so tea.Println doesn't
	// double-space the next prompt below the rendered block.
	m.appendLine(strings.TrimRight(rendered, "\n"))
	m.streaming.Reset()
}

// chromeHeight is the vertical space View() spends on non-resizable chrome
// (1 row for the header separator + 1 row for the status bar). Used by
// recomputeLayout to cap the textarea against the window total.
const chromeHeight = 2

// recomputeLayout caps the textarea height so a long paste can't push the
// status bar off-screen. With native terminal scrolling we no longer carve
// a chat viewport out of the window — the streaming preview just sits
// above the prompt at its natural height — so the cap is simply "leave
// minViewport rows of breathing room above the textarea". Cheap enough to
// run on every key press.
func (m *Model) recomputeLayout() {
	m.ta.SetHeight(max(1, min(m.visualPromptLines(), m.maxTextareaHeight())))
}

// maxTextareaHeight is the vertical cap available to the textarea — the
// terminal minus chrome, the streaming/chat breathing room, and any active
// popover. Shared by recomputeLayout (the actual SetHeight call) and
// preGrowTextarea (the pre-key inflation that keeps YOffset anchored).
func (m *Model) maxTextareaHeight() int {
	if m.height <= 0 {
		return 1
	}
	return max(1, m.height-minViewport-chromeHeight-m.popoverHeight())
}

// preGrowTextarea inflates the textarea's Height to the available cap
// *before* a KeyMsg is processed. bubbles/textarea calls repositionView()
// at the end of its Update — that scrolls the internal viewport down
// whenever the cursor moves below the current Height (e.g. a typed char
// wraps the line). Without this pre-grow, recomputeLayout (which runs
// AFTER handleKey) sets the right Height for the new content but the
// viewport stays anchored at the already-scrolled YOffset, hiding the
// earliest wrap rows. By bumping Height up first, the cursor stays inside
// the visible band for any normal keystroke and no scroll fires.
// recomputeLayout right after handleKey shrinks Height back down to the
// actual visualPromptLines, leaving YOffset at 0 and every wrapped row
// visible from the top.
func (m *Model) preGrowTextarea() {
	cap := m.maxTextareaHeight()
	if cap > m.ta.Height() {
		m.ta.SetHeight(cap)
	}
}

// visualPromptLines counts the *visual* rows the textarea needs, not the
// logical line count — a long single line that word wraps to three screen
// rows should produce a three row textarea. Uses wrapRows which mirrors
// bubbles/textarea's internal wrap() so the auto grow size matches exactly
// what the textarea renders (no off by one at word boundaries or at the
// cursor anchor trailing row). Reads DisplayValue so chip labels count as
// one line, not the hundreds of lines their expanded content would.
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
// auto grow stays in lock step with what the textarea actually renders:
// word boundary aware with a hard wrap fallback for over wide words, plus
// the trailing cursor anchor row when content exactly fills the width.
// Adapted from charmbracelet/bubbles v0.20 textarea/textarea.go:1394.
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
			// Space-less word reached a size where the next rune wouldn't
			// fit — matches bubbles' StringWidth(word)+lastCharLen check.
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

// View renders only the live region at the bottom of the terminal: any
// in-flight streaming tokens, the popover, a divider, the prompt, and the
// status bar. Everything else — past replies, banners, tool-call lines —
// has already been pushed into terminal scrollback via tea.Println, so
// the user scrolls it with their terminal's own wheel/PgUp, exactly like
// any other shell session.
func (m Model) View() string {
	if m.width == 0 || m.suppressView {
		// No WindowSizeMsg yet, or a width-resize is mid-drag. Either
		// way an empty frame is the safe answer — a collapsed 0-wide
		// layout flashes garbled, and a real frame mid-drag races the
		// renderer's stale-flush window.
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
	// "\n" instead of lipgloss.JoinVertical's Left-pad: any rendered line
	// that ends exactly in the terminal's last column triggers Apple
	// Terminal.app's last-column-wrap (DECAWM xn) inconsistently, which
	// drifts bubbletea's inline-renderer line count by one per frame —
	// visible as a duplicated prompt line that overwrites the status bar
	// on macOS while xterm/Windows-Terminal/iTerm2 stay clean. Keeping the
	// last column blank sidesteps the divergence on every rendered row.
	pieces = append(pieces,
		styleDim.Render(strings.Repeat("─", max(m.width-1, 1))),
		m.ta.View(),
		m.renderStatusBar(),
	)
	return strings.Join(pieces, "\n")
}

// splashLines builds the identity block for tea.Println. Below
// wordmarkWidth the ASCII art soft-wraps into garbage, so collapse to
// plain text rather than picking a middle variant — simpler is more
// robust here.
func (m Model) splashLines() []string {
	const wordmarkWidth = 70 // cells needed for CODE+HAMR side-by-side
	if m.width >= wordmarkWidth {
		lines := []string{""}
		for i := range splashCode {
			lines = append(lines, "  "+styleDim.Render(splashCode[i])+styleHamr.Render(splashHamr[i]))
		}
		lines = append(lines,
			"", styleDim.Render("  it's hamr time!"),
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
		styleDim.Render("  Sandboxed AI shell — run in a devcontainer or VM."),
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
	}
	if m.status != "" {
		segs = appendStatus(segs, m.status)
	}
	return strings.Join(segs, sep)
}

// appendStatus wraps one segment in styleStatus and appends it. Exists
// only to keep renderStatusBar readable — the call site was repeating
// the same `styleStatus.Render(...)` wrap six times.
func appendStatus(segs []string, s string) []string {
	return append(segs, styleStatus.Render(s))
}
