package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// quitArmText is the status-bar hint shown after the first Ctrl+C in idle.
// Lives as a const so the matching arm/disarm sites can compare against the
// same string without going out of sync.
const quitArmText = "press Ctrl+C again to quit"

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key that isn't Ctrl+C clears a pending quit arm — no stray quits.
	if msg.Type != tea.KeyCtrlC && !m.quitArmedAt.IsZero() {
		m.quitArmedAt = time.Time{}
		if m.status == quitArmText {
			m.status = ""
		}
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		return m.handleCtrlC()
	case tea.KeyCtrlL:
		// Match Claude Code: clear the typed prompt and force a full
		// terminal redraw. Conversation scrollback and history stay —
		// Ctrl+L is "tidy my input", not "start over". /clear is the
		// nuclear option.
		m.ta.Reset()
		return m, tea.ClearScreen
	case tea.KeyCtrlD:
		// Unix-standard: Ctrl+D on empty input = EOF = quit. On non-empty
		// input it's a no-op so a reflexive press never destroys a draft.
		if m.ta.Value() == "" {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyUp:
		if m.popoverOpen() {
			return m.popoverMoveSelection(-1)
		}
		// ↑ is prompt-only: cursor up if there's a row above, else
		// walk history. The terminal owns scrollback now (PgUp / mouse
		// wheel work natively), so no chat-scroll branch here.
		if !m.cursorOnFirstLine() {
			break
		}
		return m.historyUp(), nil
	case tea.KeyDown:
		if m.popoverOpen() {
			return m.popoverMoveSelection(1)
		}
		if !m.cursorOnLastLine() {
			break
		}
		return m.historyDown(), nil
	case tea.KeyTab:
		return m.handleTab(msg)
	case tea.KeyShiftTab:
		if !m.popoverOpen() {
			break
		}
		return m.popoverMoveSelection(-1)
	case tea.KeyEsc:
		if m.popoverOpen() {
			return m.handleEscInPopover()
		}
	case tea.KeyEnter:
		return m.handleEnter(msg)
	}
	return m.forwardToTextarea(msg)
}

// forwardToTextarea passes msg through to the wrapped textarea and refreshes
// the popover to match the new value. The "let the textarea handle it"
// fallback for handleKey, handleTab, and Alt+Enter — three call sites that
// were spelling the same three lines out by hand.
func (m Model) forwardToTextarea(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.refreshSuggest("")
	return m, cmd
}

// setPromptText overwrites the textarea contents, parks the cursor at the
// end, and refreshes the popover so it tracks the new value. Centralises
// the SetValue + CursorEnd + refreshSuggest dance shared by Tab completion,
// Enter advance into the arg popover, and Esc back out of it.
func (m *Model) setPromptText(s string) {
	m.ta.SetValue(s)
	m.ta.CursorEnd()
	m.refreshSuggest("")
}

// handleCtrlC implements Ctrl+C's three level precedence: in flight cancel
// beats popover close, popover close beats quit arming. Each level fully
// handles the key and never falls through.
func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.cancel != nil {
		// Whatever streamed in before Ctrl+C stays visible — abortTurn
		// flushes the partial block through the renderer so the user
		// keeps the context they had, drains turn stats so the next
		// turn's banner stays clean, then unwinds the per-turn context.
		m.abortTurn(styleWarn.Render("✗ cancelled"))
		m.quitArmedAt = time.Time{}
		m.status = ""
		return m, nil
	}
	if m.popoverOpen() {
		m.closePopover()
		m.quitArmedAt = time.Time{}
		m.status = ""
		return m, nil
	}
	if !m.quitArmedAt.IsZero() && time.Now().Before(m.quitArmedAt) {
		return m, tea.Quit
	}
	m.quitArmedAt = time.Now().Add(3 * time.Second)
	m.status = quitArmText
	return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return quitArmResetMsg{} })
}

// historyUp walks one step toward older entries; caller gates on
// cursor-on-first-line and popover closed. Empty history is a no op.
func (m Model) historyUp() Model {
	if len(m.promptHistory) == 0 {
		return m
	}
	if m.histIdx+1 < len(m.promptHistory) {
		m.histIdx++
	}
	m.ta.Restore(m.promptHistory[len(m.promptHistory)-1-m.histIdx])
	return m
}

// historyDown walks one step toward newer entries; -1 is the live draft
// sentinel and restores an empty textarea.
func (m Model) historyDown() Model {
	if m.histIdx == -1 {
		return m
	}
	m.histIdx--
	if m.histIdx == -1 {
		m.ta.Reset()
	} else {
		m.ta.Restore(m.promptHistory[len(m.promptHistory)-1-m.histIdx])
	}
	return m
}

// handleTab implements the three Tab behaviours: seed "/" on empty prompt
// (opens the command popover), complete the single remaining suggestion
// when popover open + unique match, or cycle the selection. Falls through
// to the textarea for non-empty non-popover Tabs so nothing swallows a
// user initiated indent.
func (m Model) handleTab(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.popoverOpen() {
		if m.ta.Value() == "" {
			m.setPromptText("/")
			return m, nil
		}
		return m.forwardToTextarea(msg)
	}
	// At command-level with exactly one match, Tab completes the textarea
	// to that command's name and — if the command takes args — appends a
	// space so the arg popover opens on the next refresh. Otherwise Tab
	// cycles the selection (zsh style).
	if !m.suggestArgLevel && len(m.suggest) == 1 {
		sel := m.suggest[0]
		tail := ""
		if c := commandByName(sel.value); c != nil && c.args != nil {
			tail = " "
		}
		m.setPromptText(sel.value + tail)
		return m, nil
	}
	return m.popoverMoveSelection(1)
}

// handleEscInPopover implements Esc inside the popover: arg level goes one
// step back to the command menu; command level closes the popover and
// clears the textarea.
func (m Model) handleEscInPopover() (tea.Model, tea.Cmd) {
	if m.suggestArgLevel {
		// Drop the trailing space and any typed arg prefix so refreshSuggest
		// lands on the command level list filtered to the command we were in.
		cmdName, _, _ := strings.Cut(m.ta.Value(), " ")
		m.setPromptText(cmdName)
		return m, nil
	}
	m.ta.Reset()
	m.closePopover()
	return m, nil
}

// handleEnter implements the four way Enter dispatch. Alt+Enter inserts a
// newline; otherwise the popover state decides: command level on an args
// taking command advances to the arg popover (same mental model as Tab);
// keepOpen arg level runs the handler in place and restores the arg popover;
// plain Enter commits. Factored out of handleKey because this one branch
// carries more state interaction than the other keys combined.
func (m Model) handleEnter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Alt {
		return m.forwardToTextarea(msg)
	}
	if m.phase.active() {
		return m, nil
	}
	sel, hasSel := m.currentSuggestion()
	// Command-level Enter on an args-taking command: advance to the arg
	// popover (same shape as Tab on a unique match).
	if hasSel && !m.suggestArgLevel {
		if c := commandByName(sel.value); c != nil && c.args != nil {
			m.setPromptText(sel.value + " ")
			return m, nil
		}
	}
	// Arg-level Enter on a keepOpen command: run the handler in place and
	// restore the arg popover so the user can chain toggles. The popover
	// is re-seeded with the just-confirmed value as preferred so the
	// cursor stays on that row across refreshes.
	if hasSel && m.suggestArgLevel {
		if c := commandByName(m.activeCmd); c != nil && c.keepOpen {
			next, cmd := c.handler(m, []string{sel.value})
			nm := next.(Model)
			nm.ta.SetValue(c.name + " ")
			nm.ta.CursorEnd()
			nm.refreshSuggest(sel.value)
			return nm, cmd
		}
	}
	// Plain commit. Value() expands chip labels back to full paste content
	// (→ LLM); DisplayValue() keeps labels collapsed (→ echo + history). The
	// popover selection overrides both so typing a command prefix + Enter
	// submits the full command cleanly and no chips leak into a slash command.
	var sendText, echoText string
	var entry promptEntry
	if hasSel {
		if m.suggestArgLevel {
			sendText = m.activeCmd + " " + sel.value
		} else {
			sendText = sel.value
		}
		echoText = sendText
		entry = promptEntry{display: sendText}
	} else {
		sendText = strings.TrimSpace(m.ta.Value())
		echoText = strings.TrimSpace(m.ta.DisplayValue())
		entry = m.ta.Entry()
	}
	if sendText == "" {
		return m, nil
	}
	m.ta.Reset()
	m.closePopover()
	return m.submit(sendText, echoText, entry)
}
