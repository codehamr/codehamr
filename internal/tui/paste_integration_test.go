package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestModelBracketedPasteCreatesChip: end-to-end — a bracketed-paste KeyMsg
// dispatched through Model.Update must reach promptInput.Update and produce
// a chip. Guards against a regression where the paste is swallowed by some
// earlier handleKey branch.
func TestModelBracketedPasteCreatesChip(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	paste := strings.Repeat("x\n", 20) + "end"
	pasteMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste), Paste: true}

	out, _ := m.Update(pasteMsg)
	om := out.(Model)
	if len(om.ta.spans) != 1 {
		t.Fatalf("paste via Model.Update should create a chip; spans=%+v", om.ta.spans)
	}
	if !strings.HasPrefix(om.ta.DisplayValue(), "[Pasted text +") {
		t.Fatalf("display value should start with chip label, got %q", om.ta.DisplayValue())
	}
}
