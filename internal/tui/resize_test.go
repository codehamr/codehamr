package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/mcp"
)

// TestResizeKeepsPromptInsideTerminal: in inline mode View() renders only
// the live region (optional streaming preview, popover, divider, prompt,
// status bar). Across a resize sequence the View must never claim more
// rows than the terminal — otherwise the textarea would push the status
// bar past the bottom edge or wrap mid-prompt.
func TestResizeKeepsPromptInsideTerminal(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, mcp.NewManager(), llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
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
