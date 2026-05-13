package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeChars feeds each rune of s as a separate KeyMsg through model.Update
// AND calls model.View() after each tick. Calling View() is non-cosmetic:
// bubbles' viewport only populates its internal `lines` slice inside
// View() (via SetContent), and YOffset is clamped against len(lines). A
// test that batches all keys without rendering between them keeps
// maxYOffset at 0 and never reproduces real-world scroll bugs. Real
// bubbletea runs View() after every Update, so this helper mirrors that.
func typeChars(model tea.Model, s string) tea.Model {
	for _, r := range s {
		out, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = out
		_ = model.View()
	}
	return model
}

// TestTypingPreservesEarlyContent: type a marker at the START of a long
// line, then enough filler that the line wraps to multiple visual rows.
// The marker MUST stay visible in the rendered textarea View.
//
// Bug this guards against: bubbles/textarea calls repositionView at the
// end of its Update, scrolling the internal viewport down whenever the
// cursor crosses below the current Height. recomputeLayout in this
// package grew Height *after* that scroll fired — leaving YOffset > 0
// with the first wrap row (containing the marker) clipped off the top.
// preGrowTextarea before the key reaches the textarea anchors the cursor
// inside the viewport so repositionView never scrolls.
func TestTypingPreservesEarlyContent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	final := typeChars(tea.Model(m), "ZZZSTART"+strings.Repeat("a", 300))
	mm := final.(Model)
	view := mm.ta.View()
	if !strings.Contains(view, "ZZZSTART") {
		t.Fatalf("first chars typed are not visible — viewport scrolled past them.\n"+
			"ta height=%d visualLines=%d width=%d\nView:\n%s",
			mm.ta.Height(), mm.visualPromptLines(), mm.width, view)
	}
}

// TestTypingOverflowKeepsCursorVisible: when typed content overflows the
// available textarea height (visualPromptLines > maxTextareaHeight), the
// pre-grow inflate must NOT pin the viewport to the top — the cursor
// would scroll off the bottom and the user would type blindly. In the
// overflow case the textarea's natural bottom-anchored scroll is exactly
// what we want, so verify the LAST char typed is visible (cursor row).
func TestTypingOverflowKeepsCursorVisible(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Force a small cap: height=10, minViewport=5, chrome=2 → maxTA=3.
	out, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 10})
	// Five distinct wrapped rows. ~90 chars per wrap at width 100 keeps
	// the row count predictable for a width-aware reader.
	wide := strings.Repeat("a", 90)
	body := "ROW0START" + wide + wide + wide + wide + "ROW4END"
	final := typeChars(out, body)
	mm := final.(Model)
	if mm.maxTextareaHeight() >= 5 {
		t.Fatalf("precondition: maxTextareaHeight < 5 needed to force overflow, got %d",
			mm.maxTextareaHeight())
	}
	view := mm.ta.View()
	if !strings.Contains(view, "ROW4END") {
		t.Fatalf("end of overflowing content not visible — cursor scrolled off.\n"+
			"maxTA=%d ta height=%d visualLines=%d width=%d\nView:\n%s",
			mm.maxTextareaHeight(), mm.ta.Height(), mm.visualPromptLines(), mm.width, view)
	}
}
