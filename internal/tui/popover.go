package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// popoverOpen returns true when the slash-autocomplete popover should render
// and consume ↑/↓/Tab/Esc. Closed state is every other moment.
func (m Model) popoverOpen() bool { return m.suggestOpen }

// currentSuggestion returns the highlighted popover row, or false when the
// popover is closed or empty. Surfaces the "look at the popover selection"
// pattern that handleEnter needs in three places.
func (m Model) currentSuggestion() (argOption, bool) {
	if !m.suggestOpen || len(m.suggest) == 0 {
		return argOption{}, false
	}
	return m.suggest[m.suggestIdx], true
}

// popoverMoveSelection wraps the selection index modulo the filtered list.
func (m Model) popoverMoveSelection(delta int) (tea.Model, tea.Cmd) {
	if len(m.suggest) == 0 {
		return m, nil
	}
	m.suggestIdx = (m.suggestIdx + delta + len(m.suggest)) % len(m.suggest)
	return m, nil
}

// refreshSuggest recomputes the popover from the current textarea content.
// Two levels share this one entry point — command level before the first
// space, argument level after it. When preferred is non-empty the selection
// is forced to land on that value if it still appears in the refreshed list
// (used after a keep-open Enter so the cursor stays on the row just confirmed).
func (m *Model) refreshSuggest(preferred string) {
	text := m.ta.Value()
	if strings.Contains(text, "\n") || !strings.HasPrefix(text, "/") {
		m.closePopover()
		return
	}

	cmdName, rest, hasSpace := strings.Cut(text, " ")
	if !hasSpace {
		var ms []argOption
		for _, c := range commands {
			if strings.HasPrefix(c.name, text) {
				ms = append(ms, argOption{value: c.name, description: c.description})
			}
		}
		m.setPopover(ms, false, "", "")
		return
	}

	c := commandByName(cmdName)
	if c == nil || c.args == nil {
		m.closePopover()
		return
	}
	argPrefix := strings.TrimLeft(rest, " ")
	var ms []argOption
	for _, o := range c.args(*m) {
		if strings.HasPrefix(o.value, argPrefix) {
			ms = append(ms, o)
		}
	}
	m.setPopover(ms, true, cmdName, preferred)
}

// setPopover swaps in a new set of suggestions. Selection priority: the row
// matching preferred if non-empty (used after a keep-open Enter to hold the
// cursor on the value the user just confirmed), else the `current` row (e.g.
// the active profile), else the first row.
func (m *Model) setPopover(ms []argOption, argLevel bool, cmdName, preferred string) {
	if len(ms) == 0 {
		m.closePopover()
		return
	}
	m.suggest = ms
	m.suggestArgLevel = argLevel
	m.activeCmd = cmdName
	m.suggestOpen = true
	m.suggestIdx = selectInitialIdx(ms, preferred)
}

// selectInitialIdx picks the starting row when a popover opens. Preferred
// wins if present, else the row marked current, else the first row.
func selectInitialIdx(ms []argOption, preferred string) int {
	if preferred != "" {
		for i, o := range ms {
			if o.value == preferred {
				return i
			}
		}
	}
	for i, o := range ms {
		if o.current {
			return i
		}
	}
	return 0
}

func (m *Model) closePopover() {
	m.suggestOpen = false
	m.suggest = nil
	m.suggestIdx = 0
	m.suggestArgLevel = false
	m.activeCmd = ""
}

// popoverHeight is the number of rows the popover occupies in View(). 0 when
// closed, capped so the viewport keeps breathing room.
func (m Model) popoverHeight() int {
	if !m.suggestOpen {
		return 0
	}
	return min(len(m.suggest), popoverCap)
}

// renderPopover draws the suggestion list: value flush left, description
// right aligned to the popover width, one row per suggestion. Selection is a
// colour change — bold + accent orange — via stylePopoverSelected; the
// "current" row (e.g. the active profile) is bold, no colour. No marker, no
// background, no box: the list reads as text with a highlighted row, which
// is the cleanest thing the terminal can render.
func (m Model) renderPopover() string {
	if !m.suggestOpen {
		return ""
	}
	rows := m.suggest[:m.popoverHeight()]
	var b strings.Builder
	for i, c := range rows {
		vw := lipgloss.Width(c.value)
		dw := lipgloss.Width(c.description)
		gap := max(m.width-vw-dw, 1)
		line := c.value + strings.Repeat(" ", gap) + c.description
		switch {
		case i == m.suggestIdx:
			line = stylePopoverSelected.Render(line)
		case c.current:
			line = stylePopoverCurrent.Render(line)
		default:
			line = stylePopoverRow.Render(line)
		}
		b.WriteString(line)
		if i < len(rows)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
