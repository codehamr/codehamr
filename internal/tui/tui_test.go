package tui

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codehamr/codehamr/internal/cloud"
	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
)

// newTestModel wires a model against a mock OpenAI SSE server so we can
// exercise submit → stream → done without the real stack. The server is
// torn down via t.Cleanup; callers never need to handle it directly.
func newTestModel(t *testing.T, handler http.HandlerFunc) Model {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg, _, err := config.Bootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ActiveProfile().URL = srv.URL
	client := llm.New(srv.URL, cfg.ActiveProfile().LLM, "")
	m := New(cfg, client, t.TempDir(), "test")
	// give it a size so view() doesn't panic
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return sized.(Model)
}

// TestSystemPromptIncludesWorkingDirAndInvestigateRule: the system prompt
// handed to the LLM must (a) include the embedded PROMPT_SYS.md rule that
// tells the model to investigate files first, and (b) end with the working
// directory so "hier" / "here" resolves to a concrete path.
func TestSystemPromptIncludesWorkingDirAndInvestigateRule(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	projectDir := "/workspaces/codehamr"
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), projectDir, "test")
	if !strings.Contains(m.system, "investigate first") {
		t.Fatalf("system prompt missing the 'investigate first' rule:\n%s", m.system)
	}
	if !strings.Contains(m.system, "Working directory: "+projectDir) {
		t.Fatalf("system prompt missing working-directory anchor for %q:\n%s",
			projectDir, m.system)
	}
}

// TestCtrlDEmptyQuits: Ctrl+D on empty textarea returns a Quit command.
func TestCtrlDEmptyQuits(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.ta.Value() != "" {
		t.Fatal("precondition: textarea empty")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if cmd == nil {
		t.Fatal("Ctrl+D on empty input should return tea.Quit")
	}
}

// TestCtrlDNonEmptyNoOp: Ctrl+D with text in the textarea is a no-op — no
// quit and no character deletion.
func TestCtrlDNonEmptyNoOp(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.ta.SetValue("half-written prompt")
	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if cmd != nil {
		t.Fatal("Ctrl+D with non-empty input must not quit")
	}
	if got := out.(Model).ta.Value(); got != "half-written prompt" {
		t.Fatalf("textarea was modified: %q", got)
	}
}

// TestPlaceholderMentionsTab: placeholder names both "/" and "Tab" as entry
// points into the popover, so new users discover either way.
func TestPlaceholderMentionsTab(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	view := m.View()
	if !strings.Contains(view, "/ or Tab for commands") {
		t.Fatalf("placeholder should mention both / and Tab: %s", view)
	}
	if strings.Contains(view, "/models") || strings.Contains(view, "/clear") {
		t.Fatalf("placeholder still enumerates commands: %s", view)
	}
}

// TestCtrlCIdleArmsThenQuits: first Ctrl+C in idle arms the status bar and
// returns a Tick cmd; second Ctrl+C before the window expires returns Quit.
func TestCtrlCIdleArmsThenQuits(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	first, cmd1 := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	fm := first.(Model)
	if fm.quitArmedAt.IsZero() {
		t.Fatal("first Ctrl+C should arm quitArmedAt")
	}
	if !strings.Contains(fm.renderStatusBar(), "press Ctrl+C again") {
		t.Fatalf("status bar should show arming hint, got: %s", fm.renderStatusBar())
	}
	if cmd1 == nil {
		t.Fatal("first press should return tea.Tick cmd")
	}
	_, cmd2 := fm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd2 == nil {
		t.Fatal("second Ctrl+C should return tea.Quit cmd")
	}
}

// TestCtrlCPopoverClosesInsteadOfQuitting: with the popover open and no
// in-flight op, Ctrl+C dismisses the popover and does not arm quit.
func TestCtrlCPopoverClosesInsteadOfQuitting(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	if !mm.popoverOpen() {
		t.Fatal("precondition: popover should be open")
	}
	out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	om := out.(Model)
	if om.popoverOpen() {
		t.Fatal("Ctrl+C should close popover")
	}
	if !om.quitArmedAt.IsZero() {
		t.Fatal("popover-close should not arm quit")
	}
}

// TestCtrlCCancelsInflightOp: if m.cancel is set (a turn is in flight),
// Ctrl+C calls cancel, clears pending tool calls, drops waiting, and leaves
// a "✗ cancelled" line in the scrollback.
func TestCtrlCCancelsInflightOp(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCtx = ctx
	m.cancel = cancel
	m.phase = phaseThinking
	m.pending = []chmctx.ToolCall{{Name: "bash"}}

	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	om := out.(Model)

	if om.phase.active() {
		t.Fatalf("phase should be idle after cancel, got %v", om.phase)
	}
	if om.cancel != nil {
		t.Fatal("cancel should be cleared after use")
	}
	if len(om.pending) != 0 {
		t.Fatalf("pending should be cleared: %+v", om.pending)
	}
	if !strings.Contains(om.scroll.String(), "cancelled") {
		t.Fatalf("expected ✗ cancelled in scrollback: %s", om.scroll.String())
	}
	select {
	case <-ctx.Done():
		// ctx propagated cancel — good
	default:
		t.Fatal("underlying context was not cancelled")
	}
}

// TestNonCtrlCKeypressResetsArming: once arming is live, pressing anything
// other than Ctrl+C clears the arm so the next idle Ctrl+C re-arms cleanly
// (no accidental quits).
func TestNonCtrlCKeypressResetsArming(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	first, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	fm := first.(Model)
	if fm.quitArmedAt.IsZero() {
		t.Fatal("precondition: quit should be armed")
	}
	typed, _ := fm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !typed.(Model).quitArmedAt.IsZero() {
		t.Fatal("any other keystroke must clear arming")
	}
}

// typeInto feeds text through the model one rune at a time, exactly as a
// keyboard would — this exercises the refreshSuggest hook on the KeyRunes
// fall-through. Returns the updated model.
func typeInto(m Model, text string) Model {
	var mm tea.Model = m
	for _, r := range text {
		out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		mm = out
	}
	return mm.(Model)
}

func suggestNames(m Model) []string {
	out := make([]string, 0, len(m.suggest))
	for _, c := range m.suggest {
		out = append(out, c.value)
	}
	return out
}

// TestPopoverTriggersOnSlash: typing / into an empty textarea opens the
// popover with every command; typing more characters filters by prefix.
func TestPopoverTriggersOnSlash(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	if !mm.popoverOpen() {
		t.Fatal("popover should open after typing /")
	}
	if len(mm.suggest) != len(commands) {
		t.Fatalf("all commands should match empty prefix, got %d of %d",
			len(mm.suggest), len(commands))
	}
	mm2 := typeInto(mm, "mod") // "/mod"
	names := suggestNames(mm2)
	if len(names) != 1 || names[0] != "/models" {
		t.Fatalf("expected exactly /models to match /mod, got %v", names)
	}
}

// TestPopoverClosesWhenPrefixMatchesNothing: typing a prefix that no command
// satisfies closes the popover automatically (no empty frame).
func TestPopoverClosesWhenPrefixMatchesNothing(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/zzz")
	if mm.popoverOpen() {
		t.Fatalf("popover should close when no prefix matches: %+v", mm.suggest)
	}
}

// TestPopoverTabCyclesSelection: Tab moves the selection to the next row
// without touching the textarea — zsh-style cycling.
func TestPopoverTabCyclesSelection(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	if !mm.popoverOpen() || len(mm.suggest) < 2 {
		t.Fatalf("precondition: popover open with ≥2 options, got %d", len(mm.suggest))
	}
	start := mm.suggestIdx
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if mm2.(Model).suggestIdx != (start+1)%len(mm.suggest) {
		t.Fatalf("Tab should advance selection, got idx=%d (was %d)",
			mm2.(Model).suggestIdx, start)
	}
	if got := mm2.(Model).ta.Value(); got != "/" {
		t.Fatalf("textarea must not change on Tab cycle: %q", got)
	}
	if !mm2.(Model).popoverOpen() {
		t.Fatal("popover should stay open after Tab")
	}
}

// TestPopoverTabOnEmptyOpensCommandList: Tab on an empty textarea is
// equivalent to typing "/" — it opens the popover with the full command
// list. Subsequent Tabs cycle.
func TestPopoverTabOnEmptyOpensCommandList(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.ta.Value() != "" || m.popoverOpen() {
		t.Fatal("precondition: empty textarea, popover closed")
	}
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	om := mm.(Model)
	if !om.popoverOpen() {
		t.Fatal("Tab on empty should open the command popover")
	}
	if om.ta.Value() != "/" {
		t.Fatalf("textarea should be seeded with '/' got %q", om.ta.Value())
	}
	if len(om.suggest) != len(commands) {
		t.Fatalf("popover should show all commands, got %d of %d",
			len(om.suggest), len(commands))
	}
	// second Tab cycles (5 commands → selection moves from 0 to 1)
	mm2, _ := om.Update(tea.KeyMsg{Type: tea.KeyTab})
	if mm2.(Model).suggestIdx != 1 {
		t.Fatalf("second Tab should cycle to idx 1, got %d", mm2.(Model).suggestIdx)
	}
}

// TestPopoverTabCompletesUniquePrefix: a partial command with exactly one
// match — Tab extends the textarea to the full name AND, because /models
// accepts args, appends a space that flips the popover into arg-level mode.
// This is the flow "/mod<Tab>" → "/models " + arg popover opens.
func TestPopoverTabCompletesUniquePrefix(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/mod") // only /models matches
	if len(mm.suggest) != 1 {
		t.Fatalf("precondition: one match for /mod, got %d", len(mm.suggest))
	}
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	om := mm2.(Model)
	if got := om.ta.Value(); got != "/models " {
		t.Fatalf("Tab should complete to '/models ' (with trailing space), got %q", got)
	}
	if !om.suggestArgLevel || om.activeCmd != "/models" {
		t.Fatalf("popover should have transitioned to arg-level for /models: "+
			"level=%v cmd=%q", om.suggestArgLevel, om.activeCmd)
	}
}

// TestPopoverTabOnClearCommandHasNoArgSpace: /clear takes no args, so Tab
// completes to "/clear" WITHOUT a trailing space.
func TestPopoverTabOnClearCommandHasNoArgSpace(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/cl") // only /clear matches
	if len(mm.suggest) != 1 {
		t.Fatalf("precondition: one match for /cl, got %d", len(mm.suggest))
	}
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := mm2.(Model).ta.Value(); got != "/clear" {
		t.Fatalf("Tab should complete to '/clear' (no trailing space for no-arg cmds), got %q", got)
	}
}

// TestPopoverShiftTabCyclesUp: Shift+Tab walks the selection backwards,
// wrapping at the top.
func TestPopoverShiftTabCyclesUp(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	n := len(mm.suggest)
	if !mm.popoverOpen() || n < 2 {
		t.Fatalf("precondition: popover open with ≥2 options, got %d", n)
	}
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if mm2.(Model).suggestIdx != (mm.suggestIdx-1+n)%n {
		t.Fatalf("Shift+Tab should move selection up, got idx=%d", mm2.(Model).suggestIdx)
	}
}

// TestEscFromCommandLevelClosesAndClears: Esc at command-level closes the
// popover AND clears the textarea, so the user returns to a blank prompt.
// Typing "/" from the blank slate re-opens the popover from scratch.
func TestEscFromCommandLevelClosesAndClears(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	om := mm2.(Model)
	if om.popoverOpen() {
		t.Fatal("Esc should close popover")
	}
	if om.ta.Value() != "" {
		t.Fatalf("Esc at command-level should clear textarea, got %q", om.ta.Value())
	}
	// Typing "/" from a blank slate re-opens the popover.
	mm3 := typeInto(om, "/")
	if !mm3.popoverOpen() {
		t.Fatal("typing '/' after Esc should re-open popover")
	}
}

// TestPopoverArrowKeysMoveSelection: while popover is open, ↑/↓ move the
// selection (NOT arrow history), and the textarea is not clobbered.
func TestPopoverArrowKeysMoveSelection(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.promptHistory = []promptEntry{{display: "old prompt"}} // should NOT be recalled while popover open
	mm := typeInto(m, "/")
	start := mm.suggestIdx
	n := len(mm.suggest)
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyDown})
	if mm2.(Model).suggestIdx != (start+1)%n {
		t.Fatalf("↓ should move selection, got idx=%d (was %d)",
			mm2.(Model).suggestIdx, start)
	}
	if got := mm2.(Model).ta.Value(); got != "/" {
		t.Fatalf("textarea must not be overwritten by history while popover open: %q", got)
	}
}

// TestPopoverEnterAdvancesIntoArgsForArgsCommand: Enter at command-level on
// a command that takes args does NOT submit — it opens the arg-level popover
// so the user can pick a value there. Same mental model as Tab.
func TestPopoverEnterAdvancesIntoArgsForArgsCommand(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/mod") // popover has only /models (has args)
	if !mm.popoverOpen() {
		t.Fatal("popover should be open")
	}
	out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	om := out.(Model)
	if om.ta.Value() != "/models " {
		t.Fatalf("Enter should advance textarea to '/models ', got %q", om.ta.Value())
	}
	if !om.suggestArgLevel || om.activeCmd != "/models" {
		t.Fatalf("Enter should open arg-popover for /models, got level=%v cmd=%q",
			om.suggestArgLevel, om.activeCmd)
	}
	// scroll should NOT contain a submitted /models — nothing has been sent yet
	if strings.Contains(om.scroll.String(), "▌ /models") {
		t.Fatalf("Enter must not have submitted — scroll: %s", om.scroll.String())
	}
}

// TestPopoverEnterSubmitsNoArgCommand: Enter at command-level on a command
// without args still submits immediately (/clear).
func TestPopoverEnterSubmitsNoArgCommand(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/cl") // /clear matches, no args
	if !mm.popoverOpen() {
		t.Fatal("popover should be open")
	}
	out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	om := out.(Model)
	if om.popoverOpen() {
		t.Fatal("popover should close after submit")
	}
	if om.ta.Value() != "" {
		t.Fatalf("textarea should reset: %q", om.ta.Value())
	}
	// /clear fires a "✓ conversation reset" line into scrollback
	if !strings.Contains(om.scroll.String(), "conversation reset") {
		t.Fatalf("/clear should have executed: %s", om.scroll.String())
	}
}

// TestArgPopoverOpensForModels: typing "/models " shows the profile names
// (no synthetic "next" — Tab cycles instead) with the active profile
// preselected.
func TestArgPopoverOpensForModels(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// First-run Bootstrap seeds local + hamrpass; drop the latter so this
	// test is asserting popover content, not config defaults.
	delete(m.cfg.Models, "hamrpass")
	m.cfg.Models["remote"] = &config.Profile{
		LLM: "gpt-5.1", URL: "http://r", Key: "sk-r", ContextSize: 200000,
	}
	mm := typeInto(m, "/models ")
	if !mm.suggestArgLevel || mm.activeCmd != "/models" {
		t.Fatalf("expected arg-level for /models: level=%v cmd=%q",
			mm.suggestArgLevel, mm.activeCmd)
	}
	names := suggestNames(mm)
	// sorted: [local, remote] — no "next"
	if len(names) != 2 || names[0] != "local" || names[1] != "remote" {
		t.Fatalf("expected [local remote], got %v", names)
	}
	if mm.suggest[mm.suggestIdx].value != "local" {
		t.Fatalf("default selection should be active profile 'local', got %q",
			mm.suggest[mm.suggestIdx].value)
	}
}

// TestHistoryUpDownReplayLastSubmission: ↑ on first-line replaces textarea
// with the most recent submitted line; ↓ steps back toward the draft.
func TestHistoryUpDownReplayLastSubmission(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.promptHistory = []promptEntry{{display: "first question"}, {display: "second question"}}
	m.histIdx = -1

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm.(Model).ta.Value(); got != "second question" {
		t.Fatalf("↑ should replay newest, got %q", got)
	}
	mm2, _ := mm.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm2.(Model).ta.Value(); got != "first question" {
		t.Fatalf("↑↑ should replay oldest, got %q", got)
	}
	mm3, _ := mm2.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm3.(Model).ta.Value(); got != "first question" {
		t.Fatalf("↑ past oldest should stay, got %q", got)
	}
	mm4, _ := mm3.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm4.(Model).ta.Value(); got != "second question" {
		t.Fatalf("↓ should move toward draft, got %q", got)
	}
	mm5, _ := mm4.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm5.(Model).ta.Value(); got != "" {
		t.Fatalf("↓ past newest should restore empty draft, got %q", got)
	}
	mm6, _ := mm5.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm6.(Model).ta.Value(); got != "" {
		t.Fatalf("↓ at draft should be a no-op, got %q", got)
	}
}

// TestHistoryPushesOnSubmit: successful submit appends to promptHistory and
// resets the walker index to -1.
func TestHistoryPushesOnSubmit(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	m.histIdx = 2 // simulate "was navigating history"
	mm, _ := m.submit("hello", "hello", promptEntry{display: "hello"})
	final := mm.(Model)
	if len(final.promptHistory) != 1 || final.promptHistory[0].display != "hello" {
		t.Fatalf("promptHistory wrong: %+v", final.promptHistory)
	}
	if final.histIdx != -1 {
		t.Fatalf("histIdx should reset to -1 after submit, got %d", final.histIdx)
	}
}

// TestBackendLabelShowsActiveProfile: the label echoes the currently-active
// profile name. Default Bootstrap ships one profile called "local". No
// brackets — the label is just the name (bold).
func TestBackendLabelShowsActiveProfile(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	if cfg.Active != "local" {
		t.Fatalf("default Active expected local, got %q", cfg.Active)
	}
	label := stripANSI(backendLabel(cfg, true))
	if label != "local" {
		t.Fatalf("expected plain 'local' label, got %q", label)
	}
	// second profile → label follows Active when switched
	cfg.Models["remote"] = &config.Profile{LLM: "m", URL: "http://r"}
	cfg.Active = "remote"
	if got := stripANSI(backendLabel(cfg, true)); got != "remote" {
		t.Fatalf("expected 'remote' after switch, got %q", got)
	}
}

// TestPrintHelpListsAllCommands: tui.PrintHelp formats every command in the
// central slice. Guards against a command being added to runSlash dispatch
// but forgotten in --help.
func TestPrintHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	PrintHelp(&buf)
	for _, want := range []string{"/clear", "/models", "/hamrpass"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("PrintHelp missing %q:\n%s", want, buf.String())
		}
	}
}

// TestSlashModelSwitchesActive: /models <name> sets Active and rebuilds the
// llm client's base URL / token / model to the new profile's values.
func TestSlashModelSwitchesActive(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Models["remote"] = &config.Profile{
		LLM: "gpt-5.1", URL: "http://remote:9000", Key: "sk-r", ContextSize: 200000,
	}
	m2, _ := m.runSlash("/models remote")
	final := m2.(Model)
	if final.cfg.Active != "remote" {
		t.Fatalf("Active should be 'remote', got %q", final.cfg.Active)
	}
	if final.cli.BaseURL != "http://remote:9000" {
		t.Fatalf("client.BaseURL not rebuilt: %q", final.cli.BaseURL)
	}
	if final.cli.Model != "gpt-5.1" {
		t.Fatalf("client.Model not rebuilt: %q", final.cli.Model)
	}
	if final.cli.Token != "sk-r" {
		t.Fatalf("client.Token not rebuilt: %q", final.cli.Token)
	}
}

// TestSlashModelRejectsUnknown: unknown name is a quiet warn, not a switch.
func TestSlashModelRejectsUnknown(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	before := m.cfg.Active
	m2, _ := m.runSlash("/models ghost")
	if m2.(Model).cfg.Active != before {
		t.Fatal("unknown name must not change Active")
	}
}

func TestSlashClearResetsHistory(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.history = append(m.history,
		chmctx.Message{Role: chmctx.RoleUser, Content: "hi"},
		chmctx.Message{Role: chmctx.RoleAssistant, Content: "hey"},
	)
	m2, _ := m.runSlash("/clear")
	if len(m2.(Model).history) != 0 {
		t.Fatal("/clear must drop history")
	}
}

func TestSubmitStreamsUpToDone(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	mm, cmd := m.submit("ping", "ping", promptEntry{display: "ping"})
	out, _ := drain(mm, cmd)
	if got := out.(Model).scroll.String(); !strings.Contains(got, "pong") {
		t.Fatalf("assistant output missing: %q", got)
	}
}

func TestToolCallRoundTripExecutesBash(t *testing.T) {
	// Turn 1: bash tool call. Turn 2: ask — yields cleanly under the GYSD
	// loop-tool requirement. The ask path is the test's "stop here" lever
	// without any executable verify infrastructure in test.
	turn := 0
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turn++
		switch turn {
		case 1:
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"cmd\":\"echo HAMMER\"}"}}]}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":5}}`)
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"echoed HAMMER"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"ask","arguments":"{\"question\":\"Was the echo what you wanted?\"}"}}]}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":1}}`)
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	})
	mm, cmd := m.submit("run echo", "run echo", promptEntry{display: "run echo"})
	out, _ := drain(mm, cmd)
	final := out.(Model)

	if turn != 2 {
		t.Fatalf("expected 2 LLM turns, got %d", turn)
	}
	// history: user, assistant(bash call), tool(bash result), assistant(content + ask call)
	if len(final.history) != 4 {
		t.Fatalf("history wrong: %d messages", len(final.history))
	}
	if final.history[2].Role != "tool" || !strings.Contains(final.history[2].Content, "HAMMER") {
		t.Fatalf("tool result missing: %+v", final.history[2])
	}
	if !strings.Contains(stripANSI(final.scroll.String()), "Was the echo what you wanted?") {
		t.Fatalf("ask question missing from scroll: %q", final.scroll.String())
	}
	// Per-turn summary must sum tokens across both LLM rounds, not overwrite.
	// Round 1 reports usage.completion_tokens=5, round 2 reports 1. Sum = 6.
	if !strings.Contains(stripANSI(final.scroll.String()), "6 tok") {
		t.Fatalf("per-turn summary should sum to 6 tok across rounds: %s",
			stripANSI(final.scroll.String()))
	}
}

// runTurn wires a model against handler, submits `text`, drains the resulting
// command chain, and returns the resulting Model. Shared by the status-bar
// tests below so neither duplicates the setup. token, when non-empty, is
// installed on both the active profile and the live llm.Client so cloud
// auth headers travel as in production.
func runTurn(t *testing.T, handler http.HandlerFunc, token, text string) Model {
	t.Helper()
	m := newTestModel(t, handler)
	if token != "" {
		m.cfg.ActiveProfile().Key = token
		m.cli.Token = token
	}
	mm, cmd := m.submit(text, text, promptEntry{display: text})
	out, _ := drain(mm, cmd)
	return out.(Model)
}

// budgetResponseHandler is a test LLM endpoint that answers with a single
// "ok" message plus the budget header, using OpenAI SSE format.
func budgetResponseHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Budget-Remaining", "0.73")
	fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"ok"}}]}`)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestHandleProbeSuccessUpdatesLiveCtxAndPrintsActivation: a successful
// probeMsg writes the live context window into liveContextSize (per
// profile, persisted across switches) and prints the deferred
// "✓ active: ..." line with a "ctx: ..." suffix derived from that window.
func TestHandleProbeSuccessUpdatesLiveCtxAndPrintsActivation(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Active = "hamrpass"
	out, _ := m.handleProbe(probeMsg{profile: "hamrpass", contextWindow: 262144})
	final := out.(Model)
	if got := final.liveContextSize["hamrpass"]; got != 262144 {
		t.Fatalf("liveContextSize[hamrpass] = %d, want 262144", got)
	}
	if !final.connected {
		t.Fatal("successful probe must set connected=true")
	}
	scroll := stripANSI(final.scroll.String())
	if !strings.Contains(scroll, "✓ active: hamrpass") {
		t.Fatalf("expected activation line, got:\n%s", scroll)
	}
	if !strings.Contains(scroll, "ctx: 262,144") {
		t.Fatalf("expected ctx suffix in activation line, got:\n%s", scroll)
	}
}

// TestActiveContextSizePrefersLiveValue verifies the packing path reads
// liveContextSize first, falls back to Profile.ContextSize, then to the
// hardcoded floor. Cloud profiles rely on this ordering: their on-disk
// ContextSize is 0, so without the live value the floor must apply.
func TestActiveContextSizePrefersLiveValue(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Active = "hamrpass" // ContextSize=0 by Bootstrap
	if got := m.activeContextSize(); got != defaultPackFallback {
		t.Fatalf("cloud profile with no live value should use floor %d, got %d",
			defaultPackFallback, got)
	}
	m.liveContextSize["hamrpass"] = 262144
	if got := m.activeContextSize(); got != 262144 {
		t.Fatalf("live value must win, got %d", got)
	}
}

// TestStatusBarShowsBudgetFromHeaders: the pass segment renders whenever
// the X-Budget-Remaining header arrives. The header is the only signal,
// no profile gating. The percent is rounded to a whole number so the
// readout doesn't jitter on every token.
func TestStatusBarShowsBudgetFromHeaders(t *testing.T) {
	view := runTurn(t, budgetResponseHandler, "sk-test", "hi").View()
	if !strings.Contains(view, "73% pass") {
		t.Fatalf("status bar missing pass segment: %s", view)
	}
}

// TestStatusBarOmitsBudgetWithoutHeaders: endpoint sends no budget header,
// no pass segment appears.
func TestStatusBarOmitsBudgetWithoutHeaders(t *testing.T) {
	view := runTurn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}, "", "hi").View()
	if strings.Contains(view, "pass") {
		t.Fatalf("without headers the status bar must not show pass segment: %s", view)
	}
}

// TestViewHandlesZeroWidth reproduces the "shrunk UI" startup flash: before
// WindowSizeMsg arrives, View must not panic or emit garbled layout.
func TestViewHandlesZeroWidth(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	m.width = 0 // simulate no WindowSizeMsg yet
	if got := m.View(); got != "" {
		t.Fatalf("zero-width view should be empty, got %q", got)
	}
	// After a real WindowSizeMsg the full frame should render.
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if sized.(Model).View() == "" {
		t.Fatal("sized view should not be empty")
	}
}

// TestStatusBarShowsSpinnerWhenWaiting verifies the micro-animation text
// appears in the bottom bar while a request is in flight, and disappears
// when not.
func TestStatusBarShowsSpinnerWhenWaiting(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if strings.Contains(m.renderStatusBar(), "thinking") {
		t.Fatal("idle status bar must not show thinking indicator")
	}
	m.phase = phaseThinking
	if !strings.Contains(m.renderStatusBar(), "thinking") {
		t.Fatalf("thinking status bar must show thinking indicator: %q", m.renderStatusBar())
	}
	m.phase = phaseStreaming
	if !strings.Contains(m.renderStatusBar(), "generating") {
		t.Fatalf("streaming status bar must show generating indicator: %q", m.renderStatusBar())
	}
	m.phase = phaseRunning
	if !strings.Contains(m.renderStatusBar(), "running") {
		t.Fatalf("running status bar must show running indicator: %q", m.renderStatusBar())
	}
}

// TestBackendLabelReflectsConnectedState asserts the backend label renders
// differently when connected vs not — the user's at-a-glance "are we
// talking to a server?" signal. Disconnected appends a `!` marker so the
// distinction survives on colour-stripped terminals.
func TestBackendLabelReflectsConnectedState(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	ok := backendLabel(cfg, true)
	bad := backendLabel(cfg, false)
	if ok == bad {
		t.Fatalf("connected and disconnected labels must render differently, got %q for both", ok)
	}
	if stripANSI(ok) != "local" {
		t.Fatalf("connected label should be plain profile name: %q", stripANSI(ok))
	}
	if got := stripANSI(bad); !strings.Contains(got, "local") || !strings.Contains(got, "!") {
		t.Fatalf("disconnected label must include profile name and '!' marker: %q", got)
	}
}

// TestErrorMessageUnreachable verifies the unreachable hint names the active
// profile's URL and steers the user toward /models to switch profiles.
func TestErrorMessageUnreachable(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.ActiveProfile().URL = "http://localhost:11434"

	msg := m.errorMessage(llm.Event{Err: cloud.ErrUnreachable{Err: fmt.Errorf("dial: refused")}})
	if !strings.Contains(msg, "unreachable") {
		t.Fatalf("error must say 'unreachable': %q", msg)
	}
	if !strings.Contains(msg, m.cfg.ActiveProfile().URL) {
		t.Fatalf("error must include the configured URL: %q", msg)
	}
	if !strings.Contains(msg, "/models") {
		t.Fatalf("error must hint at /models: %q", msg)
	}
	if strings.Contains(msg, "ollama") {
		t.Fatalf("error should be backend-neutral (no 'ollama'): %q", msg)
	}
}

// TestErrorMessageUnauthorized: 401 names the rejected key and the exact
// config path so the user can fix it without guessing.
func TestErrorMessageUnauthorized(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	msg := m.errorMessage(llm.Event{Err: cloud.ErrUnauthorized})
	if !strings.Contains(msg, "key rejected") {
		t.Fatalf("401 error should say 'key rejected': %q", msg)
	}
	if !strings.Contains(msg, "models."+m.cfg.Active+".key") {
		t.Fatalf("401 error should name the active profile's key path: %q", msg)
	}
}

// TestErrorMessageBudgetExhausted: 402 produces the depleted hint pointing
// users at the top-up page rather than a stack-trace style wrap.
func TestErrorMessageBudgetExhausted(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	msg := m.errorMessage(llm.Event{Err: cloud.ErrBudgetExhausted})
	if !strings.Contains(msg, "depleted") {
		t.Fatalf("402 error should mention 'depleted': %q", msg)
	}
	if !strings.Contains(msg, "codehamr.com") {
		t.Fatalf("402 error should point at the top-up page: %q", msg)
	}
}

// TestCtrlLClearsPromptNotScrollback: Ctrl+L matches Claude Code — it
// clears the typed input and forces a terminal redraw, but conversation
// scrollback stays. /clear is the only way to wipe history.
func TestCtrlLClearsPromptNotScrollback(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.ta.SetValue("half-written thought")
	m.scroll.WriteString("prior assistant message\n")

	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	om := out.(Model)

	if om.ta.Value() != "" {
		t.Fatalf("Ctrl+L must clear typed prompt, got %q", om.ta.Value())
	}
	if !strings.Contains(om.scroll.String(), "prior assistant message") {
		t.Fatalf("Ctrl+L must NOT wipe scrollback, got %q", om.scroll.String())
	}
	if cmd == nil {
		t.Fatal("Ctrl+L should return tea.ClearScreen cmd to force terminal redraw")
	}
}

// TestHumanIntFormat: thin-comma formatting must handle the edge cases the
// activation line cares about — single digits, exact 4-digit, exact powers,
// and very large windows that would otherwise read as a wall of digits.
func TestHumanIntFormat(t *testing.T) {
	cases := map[int]string{
		0:           "0",
		7:           "7",
		999:         "999",
		1000:        "1,000",
		12345:       "12,345",
		1_000_000:   "1,000,000",
		262144:      "262,144",
		8_000_000:   "8,000,000",
		262_144_000: "262,144,000",
	}
	for n, want := range cases {
		if got := humanInt(n); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestHumanTokensFormat: the session counter renders compactly — plain int
// under 1000, then `k` with an optional decimal, then `M`. Trailing `.0` is
// trimmed so round multiples read as `1k` / `10M` rather than `1.0k` /
// `10.0M`. One-format-everywhere consistency across the UI.
func TestHumanTokensFormat(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0 tok"},
		{1, "1 tok"},
		{900, "900 tok"},
		{999, "999 tok"},
		{1000, "1k tok"},
		{1200, "1.2k tok"},
		{9999, "10k tok"},
		{10_000, "10k tok"},
		{42_000, "42k tok"},
		{999_999, "1000k tok"},
		{1_000_000, "1M tok"},
		{1_500_000, "1.5M tok"},
		{12_345_678, "12.3M tok"},
	}
	for _, c := range cases {
		if got := humanTokens(c.n); got != c.want {
			t.Errorf("humanTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestHumanDurationFormat: end-of-turn elapsed renders with one decimal of
// seconds below a minute (quick turns stay informative), flips to integer
// `Xm Ys` up to an hour, then `Xh Ym`. Zero-tail segments are dropped so
// round values read as `1m` / `1h` instead of `1m 0s` / `1h 0m`.
func TestHumanDurationFormat(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0.0s"},
		{800 * time.Millisecond, "0.8s"},
		{12_300 * time.Millisecond, "12.3s"},
		{59_900 * time.Millisecond, "59.9s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m 30s"},
		{411_100 * time.Millisecond, "6m 51s"},
		{3599 * time.Second, "59m 59s"},
		{3600 * time.Second, "1h"},
		{3660 * time.Second, "1h 1m"},
		{7200 * time.Second, "2h"},
		{7500 * time.Second, "2h 5m"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestHumanRateFormat: throughput rendered as `N tok/s`. Degenerate
// inputs (zero tokens or zero elapsed) collapse to "" so the banner can
// omit the segment cleanly. Sub-10 tok/s keeps one decimal because
// reasoning models often hover near 1 tok/s where the decimal carries
// the only signal.
func TestHumanRateFormat(t *testing.T) {
	cases := []struct {
		tokens int
		d      time.Duration
		want   string
	}{
		{0, time.Second, ""},
		{10, 0, ""},
		{-3, time.Second, ""},
		{86, 3400 * time.Millisecond, "25 tok/s"},
		{50, time.Second, "50 tok/s"},
		{53, 10 * time.Second, "5.3 tok/s"},
		{1, 2 * time.Second, "0.5 tok/s"},
		{120, 120 * time.Second, "1.0 tok/s"},
		{100, time.Second, "100 tok/s"},
	}
	for _, c := range cases {
		if got := humanRate(c.tokens, c.d); got != c.want {
			t.Errorf("humanRate(%d, %v) = %q, want %q", c.tokens, c.d, got, c.want)
		}
	}
}

// TestSessionTokensAccumulateAcrossTurns: the session counter is separate
// from the per-turn counter. finalizeTurn resets turnTokens; sessionTokens
// keeps growing for the rest of the session.
func TestSessionTokensAccumulateAcrossTurns(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Simulate three Done events. phase must be active or handleStream will
	// (correctly) drop events as stale post-cancel buffer — EventDone does
	// not move phase, so we seed streaming once and let each Done carry
	// through.
	m.phase = phaseStreaming
	for _, n := range []int{7, 13, 22} {
		out, _ := m.handleStream(llm.Event{Kind: llm.EventDone, Tokens: n})
		m = out.(Model)
	}
	if m.sessionTokens != 7+13+22 {
		t.Fatalf("session counter should sum all Done events: got %d, want %d",
			m.sessionTokens, 7+13+22)
	}
	if m.turnTokens != 7+13+22 {
		// Without a streamClosedMsg between events, turnTokens keeps summing
		// too — verifying the two counters are in lockstep before finalize.
		t.Fatalf("precondition: turn counter should equal session before finalize, got %d", m.turnTokens)
	}
}

// TestSessionTokensSurviveFinalizeTurn: finalizeTurn clears turnTokens but
// must NOT touch sessionTokens.
func TestSessionTokensSurviveFinalizeTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.turnTokens = 50
	m.turnElapsed = 1 * time.Second
	m.sessionTokens = 123
	m.finalizeTurn()
	if m.turnTokens != 0 {
		t.Fatalf("turnTokens should be reset by finalizeTurn, got %d", m.turnTokens)
	}
	if m.sessionTokens != 123 {
		t.Fatalf("sessionTokens must not be touched by finalizeTurn, got %d", m.sessionTokens)
	}
}

// TestS2RepeatYieldsTurn: when the same tool call (name + canonical args)
// would be the 3rd identical attempt within MaxRecentCalls, dispatchNextTool
// must yield — pending dropped, ctx cancelled, phase=idle, scrollback explains
// the yield. This is the universal repeat-detector that replaces the per-turn
// tool-call cap; it works for bash, write_file, verify, done, and ask alike.
func TestS2RepeatYieldsTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.turnCtx = ctx
	m.cancel = cancel
	m.phase = phaseThinking
	// Pre-load the ring with two identical bash calls. The next dispatch
	// will be the 3rd and S2 should fire.
	args := map[string]any{"cmd": "echo x"}
	m.gysd.NoteToolCall("bash", args)
	m.gysd.NoteToolCall("bash", args)
	m.pending = []chmctx.ToolCall{{Name: "bash", Arguments: args}}

	out, _ := m.Update(streamClosedMsg{})
	om := out.(Model)

	if om.phase.active() {
		t.Fatalf("phase must be idle after S2 yield, got %v", om.phase)
	}
	if om.cancel != nil {
		t.Fatal("cancel must be cleared after S2 yield")
	}
	if om.turnCtx != nil {
		t.Fatal("turnCtx must be cleared after S2 yield")
	}
	if len(om.pending) != 0 {
		t.Fatalf("pending must be dropped: %+v", om.pending)
	}
	scroll := stripANSI(om.scroll.String())
	if !strings.Contains(scroll, "bash") || !strings.Contains(scroll, "repeated 3×") {
		t.Fatalf("scrollback missing S2 yield notice: %q", scroll)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("underlying context was not cancelled by S2 yield")
	}
}

// TestStatusBarShowsSessionTokens: once the counter is non-zero it appears
// in the status bar; at zero the bar stays quiet.
func TestStatusBarShowsSessionTokens(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if strings.Contains(m.renderStatusBar(), "tok") {
		t.Fatalf("fresh session (0 tok) should not render counter: %q", m.renderStatusBar())
	}
	m.sessionTokens = 1234
	bar := m.renderStatusBar()
	if !strings.Contains(bar, "1.2k tok") {
		t.Fatalf("status bar should show compact session counter: %q", bar)
	}
}

// TestClearResetsSessionTokens: /clear wipes the conversation AND the
// session counter — starting over from zero is the whole point.
func TestClearResetsSessionTokens(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.sessionTokens = 999
	out, _ := m.runSlash("/clear")
	if got := out.(Model).sessionTokens; got != 0 {
		t.Fatalf("/clear should reset sessionTokens to 0, got %d", got)
	}
}

// TestWrapRowsMatchesBubblesBehaviour: wrapRows must produce the same row
// count as bubbles/textarea's internal wrap(). Word-boundary aware (a
// wrapped space-delimited text can leave more than half the last row
// empty), hard-wrap fallback for over-wide single words, and the trailing
// cursor-anchor row when content exactly fills the width.
func TestWrapRowsMatchesBubblesBehaviour(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		width    int
		wantRows int
	}{
		{"empty", "", 15, 1},
		{"short fits", "hello", 15, 1},
		{"short with spaces fits", "hi you", 15, 1},
		{"exactly fills width adds cursor anchor", strings.Repeat("x", 15), 15, 2},
		{"just under width", strings.Repeat("x", 14), 15, 1},
		{"just over width hard-wraps", strings.Repeat("x", 16), 15, 2},
		{"long hard-wrap no spaces", strings.Repeat("x", 45), 15, 4},
		{"word-boundary wastes space", strings.Repeat("hello ", 20), 15, 10},
		{"zero width floors to 1", "anything", 0, 1},
	}
	for _, c := range cases {
		if got := wrapRows(c.input, c.width); got != c.wantRows {
			t.Errorf("wrapRows(%q, %d) = %d, want %d", c.input, c.width, got, c.wantRows)
		}
	}
}

// TestVisualPromptLinesSumsAcrossLogicalLines: visualPromptLines splits on
// newlines and sums wrapRows for each segment — the prompt field grows to
// hold the combined visual height.
func TestVisualPromptLinesSumsAcrossLogicalLines(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// width=100, effective=96. Two short logical lines = 2 visual rows.
	m.ta.SetValue("first line\nsecond line")
	if got := m.visualPromptLines(); got != 2 {
		t.Errorf("two short lines should produce 2 visual rows, got %d", got)
	}
	// One line that wraps to multiple visual rows.
	m.ta.SetValue(strings.Repeat("x", 200))
	if got := m.visualPromptLines(); got < 2 {
		t.Errorf("200-char line should wrap past 1 row, got %d", got)
	}
}

// TestPromptGrowsOnWrappedLongLine: the real-world case — user types one
// long paragraph without pressing Enter. LineCount() returns 1 for that, so
// relying on it leaves the textarea stuck at 1 row while the text wraps
// invisibly off-screen. recomputeLayout must count *visual* rows.
func TestPromptGrowsOnWrappedLongLine(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// newTestModel sets width=100 → effective text width ~96. 300 runes of
	// text wraps to 4 rows (ceil(300/96)).
	m.ta.SetValue(strings.Repeat("x", 300))
	m.recomputeLayout()
	if got := m.ta.Height(); got < 2 {
		t.Fatalf("long wrapped line should expand textarea past 1 row, got %d", got)
	}
}

// TestPromptAutoGrowsWithContent: the textarea starts at 1 line when empty,
// grows as its content gains newlines, and clamps to the dynamic cap
// (height - minViewport - 2 - popover). hamr default: no 3-line block
// hogging the frame on an empty prompt, but no 8-line ceiling either —
// big pastes get to use most of the screen while chat keeps its floor.
func TestPromptAutoGrowsWithContent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.ta.Height() != 1 {
		t.Fatalf("empty prompt should be 1 line, got %d", m.ta.Height())
	}

	m.ta.SetValue("line1\nline2\nline3\nline4")
	m.recomputeLayout()
	if got := m.ta.Height(); got != 4 {
		t.Fatalf("4 lines in textarea should produce height 4, got %d", got)
	}

	// Far past the cap — height must clamp to leave room for chat. With
	// newTestModel's 30-row terminal, cap = 30 - 5 - 2 - 0 = 23.
	m.ta.SetValue(strings.Repeat("x\n", 40) + "end")
	m.recomputeLayout()
	want := m.height - minViewport - 2
	if got := m.ta.Height(); got != want {
		t.Fatalf("height should clamp to %d (h=%d - minViewport=%d - chrome=2), got %d",
			want, m.height, minViewport, got)
	}
}

// TestPromptShrinksAfterSubmit: after Enter submits a multi-line prompt the
// textarea resets to empty and the height snaps back to 1 line.
func TestPromptShrinksAfterSubmit(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	m.ta.SetValue("line1\nline2\nline3\nline4")
	m.recomputeLayout()
	if m.ta.Height() != 4 {
		t.Fatalf("precondition: 4-line draft, got height %d", m.ta.Height())
	}
	// Enter at idle submits the current textarea value and calls ta.Reset;
	// recomputeLayout after handleKey then snaps the height back down.
	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	final, _ := drain(out, cmd)
	if got := final.(Model).ta.Height(); got != 1 {
		t.Fatalf("after submit the textarea should collapse to 1 line, got %d", got)
	}
}

// stripANSI removes CSI escape sequences (`\x1b[…m`) so tests can match the
// visible text through glamour's per-word styling, which splits "foo bar" into
// `<span>foo</span> <span>bar</span>` and breaks a naive Contains("foo bar").
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for j := i + 2; j < len(s); j++ {
				if s[j] >= 0x40 && s[j] <= 0x7e {
					i = j
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestStreamContentShowsLiveInViewport: a content event arriving mid-turn
// must populate the streaming buffer, promote phase thinking→streaming, and
// be visible in View() before any EventDone arrives. This is the whole
// "tokens stream immediately" promise from the README.
func TestStreamContentShowsLiveInViewport(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseThinking
	out, _ := m.handleStream(llm.Event{Kind: llm.EventContent, Content: "hello world"})
	om := out.(Model)
	if om.phase != phaseStreaming {
		t.Fatalf("first content chunk should promote phase to streaming, got %v", om.phase)
	}
	if om.streaming.String() != "hello world" {
		t.Fatalf("streaming buffer should contain raw content, got %q", om.streaming.String())
	}
	if om.scroll.Len() != 0 {
		t.Fatalf("scroll should still be empty before flush, got %q", om.scroll.String())
	}
	if !strings.Contains(om.View(), "hello world") {
		t.Fatalf("View must show live text during stream: %q", om.View())
	}
}

// TestEventDoneFlushesStreamingThroughGlamour: EventDone moves raw streamed
// text into the committed scroll via the Markdown renderer and empties the
// streaming buffer. After Done, scroll contains the rendered block and the
// streaming buffer is empty.
func TestEventDoneFlushesStreamingThroughGlamour(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.streaming.WriteString("done message")
	out, _ := m.handleStream(llm.Event{Kind: llm.EventDone, Final: &chmctx.Message{Role: chmctx.RoleAssistant, Content: "done message"}})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatalf("streaming should be empty after Done, got %q", om.streaming.String())
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "done message") {
		t.Fatalf("rendered content should be in scroll: %q", om.scroll.String())
	}
}

// TestToolCallFlushesStreamedContent: a tool-call event ends the current
// content phase — whatever streamed in before it is rendered and committed
// *now*, so the user sees styled text *before* the inline tool-call status
// rather than all at once at the end of the turn.
func TestToolCallFlushesStreamedContent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.streaming.WriteString("I'll run bash.")
	call := chmctx.ToolCall{ID: "c1", Name: "bash"}
	out, _ := m.handleStream(llm.Event{Kind: llm.EventToolCall, ToolCall: &call})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatal("tool-call must flush streaming buffer into scroll")
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "I'll run bash") {
		t.Fatalf("content should be committed to scroll before tool-call: %q", om.scroll.String())
	}
	if len(om.pending) != 1 || om.pending[0].Name != "bash" {
		t.Fatalf("tool-call should land in pending: %+v", om.pending)
	}
}

// TestCancelMidStreamPreservesStreamedText: Ctrl+C while content is streaming
// keeps the partial response visible (flushed to scroll) and appends the
// cancelled marker. The user never loses context they've already read.
func TestCancelMidStreamPreservesStreamedText(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCtx = ctx
	m.cancel = cancel
	m.phase = phaseStreaming
	m.streaming.WriteString("partial response before cancel")

	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	om := out.(Model)

	if om.streaming.Len() != 0 {
		t.Fatal("streaming buffer should be flushed by cancel")
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "partial response before cancel") {
		t.Fatalf("streamed text must survive cancel, got: %q", om.scroll.String())
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "cancelled") {
		t.Fatalf("cancelled marker missing from scroll: %q", om.scroll.String())
	}
	if om.phase.active() {
		t.Fatal("phase must return to idle after cancel")
	}
}

// TestHandleStreamDrainsAfterCancel: a stream goroutine's buffered events
// can still arrive after Ctrl+C has returned phase to idle. Processing them
// would write ghost tokens into scroll, re-populate m.pending, and credit
// a cancelled turn's usage to sessionTokens. handleStream must drain-only.
func TestHandleStreamDrainsAfterCancel(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseIdle // post-cancel state
	m.sessionTokens = 100

	out, _ := m.handleStream(llm.Event{Kind: llm.EventContent, Content: "ghost"})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatalf("stale EventContent wrote to streaming: %q", om.streaming.String())
	}
	out, _ = om.handleStream(llm.Event{Kind: llm.EventToolCall, ToolCall: &chmctx.ToolCall{ID: "c", Name: "bash"}})
	om = out.(Model)
	if len(om.pending) != 0 {
		t.Fatalf("stale EventToolCall re-populated pending: %+v", om.pending)
	}
	out, _ = om.handleStream(llm.Event{Kind: llm.EventDone, Tokens: 50})
	om = out.(Model)
	if om.sessionTokens != 100 {
		t.Fatalf("stale EventDone credited tokens to session: %d", om.sessionTokens)
	}
}

// TestHandleStreamClosedSkipsAdvanceAfterCancel: Ctrl+C during a turn
// leaves phase=idle; the deferred streamClosedMsg must not auto-restart
// a new turn (which would surprise the user with the loop re-nudging
// itself after they asked to stop). gysd MissingStreak should also stay put.
func TestHandleStreamClosedSkipsAdvanceAfterCancel(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseIdle // handleCtrlC already finalised
	m.gysd.MissingStreak = 1

	out, cmd := m.handleStreamClosed()
	om := out.(Model)
	if cmd != nil {
		t.Fatal("stale close after cancel must NOT return a new-turn Cmd")
	}
	if om.gysd.MissingStreak != 1 {
		t.Fatalf("MissingStreak should not tick on stale close: got %d", om.gysd.MissingStreak)
	}
}

// TestStaleStreamEventDoesNotMutateLiveTurn: after Ctrl+C kills turn 1 and
// the user submits turn 2, the prior turn's readEvent Cmd can still fire its
// next event (channel buffered or producer not yet exited). That stale event
// must not write into turn 2's streaming buffer or session counters — it
// belongs to a turn that is over.
func TestStaleStreamEventDoesNotMutateLiveTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	stale := make(chan llm.Event, 1)
	live := make(chan llm.Event, 1)
	m.stream = live
	m.phase = phaseThinking
	m.sessionTokens = 50

	out, _ := m.Update(streamEventMsg{ch: stale, e: llm.Event{Kind: llm.EventContent, Content: "ghost"}})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatalf("stale content event leaked into live turn's streaming buffer: %q", om.streaming.String())
	}
	if om.sessionTokens != 50 {
		t.Fatalf("stale event credited tokens to live session: %d", om.sessionTokens)
	}
	if om.stream != live {
		t.Fatal("stale event must not overwrite live m.stream")
	}
}

// TestStaleStreamCloseDoesNotKillLiveTurn: the prior turn's channel closing
// after a fresh submit must NOT run handleStreamClosed against the live turn.
// Doing so would (a) nil out m.stream, breaking the live read loop, and
// (b) finalizeTurn + endTurn the current request out from under the user.
func TestStaleStreamCloseDoesNotKillLiveTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	stale := make(chan llm.Event)
	live := make(chan llm.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.turnCtx = ctx
	m.cancel = cancel
	m.stream = live
	m.phase = phaseStreaming

	out, _ := m.Update(streamClosedMsg{ch: stale})
	om := out.(Model)
	if om.stream != live {
		t.Fatal("stale close must not overwrite m.stream — live read loop would die")
	}
	if !om.phase.active() {
		t.Fatalf("stale close finalised the live turn — phase is now %v", om.phase)
	}
	if om.cancel == nil {
		t.Fatal("stale close cancelled the live turn's context")
	}
}

// TestEventErrorPreservesStreamedText: a stream error mid-content flushes the
// partial text before appending the error line — same principle as cancel,
// user keeps the context they were reading.
func TestEventErrorPreservesStreamedText(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.streaming.WriteString("got this far then")
	out, _ := m.handleStream(llm.Event{Kind: llm.EventError, Err: fmt.Errorf("boom")})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatal("streaming should be flushed on error")
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "got this far then") {
		t.Fatalf("pre-error content must survive: %q", om.scroll.String())
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "boom") {
		t.Fatalf("error message should follow the content: %q", om.scroll.String())
	}
	if om.phase.active() {
		t.Fatal("phase must return to idle after error")
	}
}

// drain advances the model until no more async commands are pending. It's a
// synchronous bubbletea mini-runtime used by tests. tea.BatchMsg arrives
// when Update wraps multiple Cmds (e.g. tea.Println prints from the outbox
// + the handler's own Cmd); each child Cmd is run, its result fed back
// through Update, and any new Cmd it returns appended to the queue so the
// chain keeps unfolding.
func drain(m tea.Model, cmd tea.Cmd) (tea.Model, []tea.Msg) {
	var seen []tea.Msg
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		cmd, queue = queue[0], queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		if msg == nil {
			continue
		}
		seen = append(seen, msg)
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c == nil {
					continue
				}
				bm, bcmd := m.Update(c())
				m = bm
				queue = append(queue, bcmd)
			}
			continue
		}
		var nextCmd tea.Cmd
		m, nextCmd = m.Update(msg)
		queue = append(queue, nextCmd)
	}
	return m, seen
}

// TestPopoverRenderHasNoMarker: no row in the rendered popover is prefixed
// with the old `▸ ` arrow or a 2-space marker. Selection is a colour change,
// not a marker.
func TestPopoverRenderHasNoMarker(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	rendered := stripANSI(mm.renderPopover())
	for line := range strings.SplitSeq(rendered, "\n") {
		if strings.HasPrefix(line, "▸ ") || strings.HasPrefix(line, "  /") {
			t.Fatalf("popover row must not carry a marker prefix: %q", line)
		}
	}
}

// TestSplashEmittedOnFirstSize: in inline mode the splash is printed once
// into terminal scrollback on the first WindowSizeMsg, then it scrolls up
// naturally as content arrives. We verify the lines reach the outbox (which
// the Update wrapper drains via tea.Println in production).
func TestSplashEmittedOnFirstSize(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	out, _ := m.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	om := out.(Model)
	joined := stripANSI(strings.Join(om.outbox, "\n"))
	if !strings.Contains(joined, "hamr") {
		t.Fatalf("splash should be queued on first size: %s", joined)
	}
	if !strings.Contains(joined, "codehamr test") {
		t.Fatalf("splash should carry version/profile line: %s", joined)
	}
	if !strings.Contains(joined, "AI systems can make mistakes") {
		t.Fatalf("splash should carry AI safety notice: %s", joined)
	}
	if !strings.Contains(joined, "devcontainer or VM") {
		t.Fatalf("splash should recommend a sandbox: %s", joined)
	}
	// A second size message must not re-emit the splash. Clear the
	// captured outbox first — production drains it via tea.Println in the
	// Update wrapper, but we called update() directly here.
	om.outbox = nil
	out2, _ := om.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	om2 := out2.(Model)
	if joined2 := strings.Join(om2.outbox, "\n"); strings.Contains(joined2, "hamr time") {
		t.Fatalf("splash must be a one-shot, got re-emit: %s", joined2)
	}
}

// TestStreamingShownInLiveView: the streaming buffer is rendered above
// the prompt by View() while content is in flight, so the user sees
// tokens immediately even before the block flushes to scrollback.
func TestStreamingShownInLiveView(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.streaming.WriteString("live tokens arriving...")
	if !strings.Contains(stripANSI(m.View()), "live tokens arriving") {
		t.Fatalf("View must include streaming buffer: %s", stripANSI(m.View()))
	}
}

// TestFocusBlurMsgsAreInert: terminal focus-in / focus-out reports arrive as
// tea.FocusMsg / tea.BlurMsg when tea.WithReportFocus is enabled. They must
// never touch the textarea (no inserted chars, no height change). Reproduces
// the "UI slides up when I switch to another terminal window" bug seen when
// a parallel claude-code session next to codehamr steals focus.
func TestFocusBlurMsgsAreInert(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	beforeVal := m.ta.Value()
	beforeHeight := m.ta.Height()

	out, cmd := m.Update(tea.FocusMsg{})
	om := out.(Model)
	if om.ta.Value() != beforeVal {
		t.Fatalf("FocusMsg altered textarea: %q → %q", beforeVal, om.ta.Value())
	}
	if om.ta.Height() != beforeHeight {
		t.Fatalf("FocusMsg changed textarea height: %d → %d", beforeHeight, om.ta.Height())
	}
	if cmd != nil {
		t.Fatal("FocusMsg must not return a Cmd")
	}

	out, cmd = m.Update(tea.BlurMsg{})
	om = out.(Model)
	if om.ta.Value() != beforeVal || om.ta.Height() != beforeHeight {
		t.Fatalf("BlurMsg altered state: val=%q h=%d",
			om.ta.Value(), om.ta.Height())
	}
	if cmd != nil {
		t.Fatal("BlurMsg must not return a Cmd")
	}
}

// TestEmptyRunesKeyIsDropped: KeyMsg{Type: KeyRunes, Runes: nil} can arise
// when bubbletea's parser chokes on a partial escape sequence it doesn't
// recognize. Letting it fall through inserts nothing but still triggers
// recomputeLayout — harmless in isolation, but a parser that produces a
// stream of these makes the UI flicker. Drop them at the front door.
func TestEmptyRunesKeyIsDropped(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	beforeVal := m.ta.Value()
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: nil})
	om := out.(Model)
	if om.ta.Value() != beforeVal {
		t.Fatalf("empty-runes key leaked into textarea: %q", om.ta.Value())
	}
}

// TestPopoverRenderRightAligns: each row ends flush with the popover width
// (== m.width after ANSI stripping). The value starts at column 0 and the
// description is right-aligned.
func TestPopoverRenderRightAligns(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	rendered := stripANSI(mm.renderPopover())
	for line := range strings.SplitSeq(rendered, "\n") {
		if line == "" {
			continue
		}
		if got := lipgloss.Width(line); got != mm.width {
			t.Fatalf("row width = %d, want %d: %q", got, mm.width, line)
		}
		// value starts at column 0 — first rune is a slash
		if !strings.HasPrefix(line, "/") {
			t.Fatalf("row should start with the command name at column 0: %q", line)
		}
	}
}

// TestHamrpassNoArgsShowsExplainerWhenUnset: `/hamrpass` with no key set
// prints the guided block, including the unset status line and the
// purchase URL. No active-profile change.
func TestHamrpassNoArgsShowsExplainerWhenUnset(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	before := m.cfg.Active
	// Bootstrap seeds hamrpass with an empty key; assert the precondition
	// so the test fails loud if Default() ever changes.
	if hp, ok := m.cfg.Models["hamrpass"]; !ok || hp.Key != "" {
		t.Fatalf("precondition: hamrpass profile with empty key, got %+v", hp)
	}
	m2, _ := m.runSlash("/hamrpass")
	final := m2.(Model)
	out := stripANSI(final.scroll.String())
	for _, want := range []string{
		"hamrpass",
		"status   : unset",
		"endpoint : https://codehamr.com",
		"llm      : hamrpass",
		"https://codehamr.com",
		"/hamrpass <your key>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explainer missing %q in:\n%s", want, out)
		}
	}
	if final.cfg.Active != before {
		t.Fatalf("/hamrpass without args must not change active profile, got %q", final.cfg.Active)
	}
}

// TestHamrpassNoArgsShowsSetWhenKeyPresent: status line flips to `set`
// when the hamrpass profile already has a key.
func TestHamrpassNoArgsShowsSetWhenKeyPresent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Models["hamrpass"].Key = "hp-already-1234567890abcdef"
	m2, _ := m.runSlash("/hamrpass")
	out := stripANSI(m2.(Model).scroll.String())
	if !strings.Contains(out, "status   : set") {
		t.Fatalf("explainer should report status:set when key present:\n%s", out)
	}
}

// TestHamrpassSetsKeyAndActivates: a valid key is trimmed, saved on the
// hamrpass profile, persisted, and the active profile flips to hamrpass.
// The llm client is rebuilt so future requests carry the new token.
func TestHamrpassSetsKeyAndActivates(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.cfg.Active == "hamrpass" {
		t.Fatal("precondition: active should not be hamrpass on a fresh model")
	}
	const key = "hp-test-key-1234567890abcdef"
	m2, cmd := m.runSlash("/hamrpass " + key)
	final := m2.(Model)
	if final.cfg.Active != "hamrpass" {
		t.Fatalf("active should switch to hamrpass, got %q", final.cfg.Active)
	}
	if got := final.cfg.Models["hamrpass"].Key; got != key {
		t.Fatalf("key not stored: %q", got)
	}
	if final.cli.Token != key {
		t.Fatalf("llm client token not rebuilt: %q", final.cli.Token)
	}
	if final.cli.Model != "hamrpass" {
		t.Fatalf("llm client model not rebuilt: %q", final.cli.Model)
	}
	if cmd == nil {
		t.Fatal("set should return a probeBackend command")
	}
	// Activation now defers the success line until probeMsg arrives — the
	// synchronous scrollback shows the "▶ probing" placeholder instead.
	// Final "✓ active" line is exercised in TestHandleProbeSuccess.
	out := stripANSI(final.scroll.String())
	if !strings.Contains(out, "▶ probing hamrpass") {
		t.Fatalf("expected probing placeholder in scrollback:\n%s", out)
	}
}

// TestHamrpassLazyCreatesProfile: a user who has hidden hamrpass from
// config.yaml can still activate it by pasting a key. /hamrpass <key>
// creates the profile from canonical seed values, stores the key, and
// flips active — no "restart codehamr" detour.
func TestHamrpassLazyCreatesProfile(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	delete(m.cfg.Models, "hamrpass")
	if _, ok := m.cfg.Models["hamrpass"]; ok {
		t.Fatal("precondition: hamrpass should be absent")
	}
	const key = "hp-test-key-1234567890abcdef"
	m2, cmd := m.runSlash("/hamrpass " + key)
	final := m2.(Model)
	hp, ok := final.cfg.Models["hamrpass"]
	if !ok {
		t.Fatal("hamrpass profile should be lazy-created by /hamrpass")
	}
	if hp.URL != "https://codehamr.com" || hp.LLM != "hamrpass" {
		t.Fatalf("lazy-created hamrpass has wrong canonical fields: %+v", hp)
	}
	if hp.Key != key {
		t.Fatalf("key not stored on lazy-created profile: %q", hp.Key)
	}
	if final.cfg.Active != "hamrpass" {
		t.Fatalf("active should switch to hamrpass, got %q", final.cfg.Active)
	}
	if cmd == nil {
		t.Fatal("set should return a probeBackend command on lazy-create path too")
	}
}

// TestHamrpassRejectsTooShort: a key under hamrpassMinKeyLen is refused
// without touching the profile or the active selection.
func TestHamrpassRejectsTooShort(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	beforeActive := m.cfg.Active
	beforeKey := m.cfg.Models["hamrpass"].Key
	m2, _ := m.runSlash("/hamrpass abc")
	final := m2.(Model)
	if final.cfg.Active != beforeActive {
		t.Fatalf("active changed on rejected key: %q", final.cfg.Active)
	}
	if final.cfg.Models["hamrpass"].Key != beforeKey {
		t.Fatalf("rejected key was stored: %q", final.cfg.Models["hamrpass"].Key)
	}
	out := stripANSI(final.scroll.String())
	// New wording stays consistent with the popover hint:
	// "N/16 chars · keep typing".
	if !strings.Contains(out, "/16 chars") || !strings.Contains(out, "keep typing") {
		t.Fatalf("expected length hint in scrollback:\n%s", out)
	}
}

// TestHamrpassRejectsMultipleArgs: a paste with embedded whitespace splits
// into multiple args via strings.Fields. The handler refuses with a
// dedicated message rather than silently joining or accepting one half.
func TestHamrpassRejectsMultipleArgs(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	beforeActive := m.cfg.Active
	beforeKey := m.cfg.Models["hamrpass"].Key
	m2, _ := m.runSlash("/hamrpass hp-first-half hp-second-half")
	final := m2.(Model)
	if final.cfg.Active != beforeActive {
		t.Fatalf("active changed on rejected multi-arg key: %q", final.cfg.Active)
	}
	if final.cfg.Models["hamrpass"].Key != beforeKey {
		t.Fatalf("rejected key was stored: %q", final.cfg.Models["hamrpass"].Key)
	}
	out := stripANSI(final.scroll.String())
	if !strings.Contains(out, "cannot contain spaces") {
		t.Fatalf("expected space-rejection error in scrollback:\n%s", out)
	}
}
