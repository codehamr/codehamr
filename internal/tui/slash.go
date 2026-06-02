package tui

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/cloud"
	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
)

// argOption is one popover entry, used at command-level (one row per command)
// and argument-level (one row per accepted value for the active command).
type argOption struct {
	value       string // what gets inserted / committed
	description string // right-aligned help text
	current     bool   // rendered bold; default-selected when the popover opens
}

// command is one row in the popover, --help, and the dispatch table.
// args, if non-nil, supplies the argument-level popover entries.
type command struct {
	name        string
	description string
	handler     func(Model, []string) (tea.Model, tea.Cmd)
	args        func(Model) []argOption
}

// commands lists every slash command, in popover/--help order. Keep it short.
var commands = []command{
	{
		name:        "/hamrpass",
		description: "set or show hamrpass key",
		handler:     (Model).cmdHamrpass,
		// Live key-entry hint: selecting /hamrpass auto-inserts the trailing
		// space (handleEnter/handleTab do this whenever args != nil), then the
		// arg popover renders one synthetic row that validates the key live.
		// The row's value mirrors the input so HasPrefix always keeps it, and
		// Enter submits "/hamrpass <key>".
		args: hamrpassArgHint,
	},
	{
		name:        "/clear",
		description: "reset the conversation",
		handler:     (Model).cmdClear,
	},
	{
		name:        "/models",
		description: "list · <name> set (Tab cycles in the popover)",
		handler:     (Model).cmdModel,
		args: func(m Model) []argOption {
			out := make([]argOption, 0, len(m.cfg.Models))
			for _, n := range m.cfg.ModelNames() {
				p := m.cfg.Models[n]
				out = append(out, argOption{
					value:       n,
					description: p.LLM + " @ " + p.URL,
					current:     n == m.cfg.Active,
				})
			}
			return out
		},
	},
}

// commandByName returns the registered command for a slash name, or nil.
// Centralises the linear scan shared by completion, dispatch, and runSlash.
func commandByName(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// runSlash dispatches a slash-prefixed submission; unknown commands produce a
// quiet hint, not an error. config.yaml is re-read before every slash so
// hand-edits take effect without a restart (see reloadConfigFromDisk).
func (m Model) runSlash(text string) (tea.Model, tea.Cmd) {
	if err := m.reloadConfigFromDisk(); err != nil {
		m.appendLine(styleWarn.Render("⚠ " + err.Error()))
	}
	fields := strings.Fields(text)
	if c := commandByName(fields[0]); c != nil {
		return c.handler(m, fields[1:])
	}
	m.appendLine(styleWarn.Render("unknown command - type / to see options"))
	return m, nil
}

// reloadConfigFromDisk re-runs config.Bootstrap and replaces m.cfg so hand-edits
// to config.yaml between slash commands take effect immediately. URLOverride
// (from CODEHAMR_URL) is carried across the swap so the env var keeps applying.
//
// Returns the Bootstrap error verbatim; callers decide whether to surface it
// (runSlash warns on submit; the popover-refresh path ignores it so a broken
// file doesn't spam a warning on every keystroke).
//
// Rebuilds the llm.Client when the active profile's resolved (URL, model, key)
// triple changed: covers both within-profile edits and a moved active.
func (m *Model) reloadConfigFromDisk() error {
	projectRoot := filepath.Dir(m.cfg.Dir)
	fresh, _, err := config.Bootstrap(projectRoot)
	if err != nil {
		return err
	}
	fresh.URLOverride = m.cfg.URLOverride

	prevURL := m.cfg.ActiveURL()
	prevProfile := m.cfg.ActiveProfile()
	prevLLM, prevKey := prevProfile.LLM, prevProfile.Key

	m.cfg = fresh

	newProfile := m.cfg.ActiveProfile()
	if prevURL != m.cfg.ActiveURL() || prevLLM != newProfile.LLM || prevKey != newProfile.Key {
		m.rebuildClient()
	}
	return nil
}

// PrintHelp writes the canonical human-readable command list. Used by --help.
func PrintHelp(out io.Writer) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\t%s\n", c.name, c.description)
	}
	w.Flush()
}

// --- handlers ---------------------------------------------------------------

// cmdModel: `/models` lists, `/models <name>` sets. Cycling is Tab/Shift+Tab
// in the popover, no separate "next" command.
func (m Model) cmdModel(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.printModelList()
		return m, nil
	}
	if err := m.cfg.SetActive(args[0]); err != nil {
		m.appendLine(styleError.Render("⚠ " + err.Error()))
		return m, nil
	}
	m.rebuildClient()
	return m, m.confirmActive(args[0])
}

// printModelList writes the "▸ active, name, llm @ url" rollup to scroll.
func (m *Model) printModelList() {
	m.appendLine(styleDim.Render("models (▸ active, /models <name> to switch):"))
	for _, n := range m.cfg.ModelNames() {
		mark := "  "
		if n == m.cfg.Active {
			mark = "▸ "
		}
		p := m.cfg.Models[n]
		m.appendLine(fmt.Sprintf("%s%s  %s",
			mark, n, styleDim.Render(p.LLM+" @ "+p.URL)))
	}
}

// confirmActive emits the activation line for the active profile and returns
// its reachability cmd. Keyed profiles (cloud) probe: the success line is
// delayed until the response arrives so it can carry the live ctx window from
// X-Context-Window. Keyless profiles (local Ollama) ping and print
// synchronously. Shared by /models and /hamrpass.
func (m *Model) confirmActive(profile string) tea.Cmd {
	p := m.cfg.ActiveProfile()
	if p.Key != "" {
		m.appendLine(styleDim.Render(fmt.Sprintf("▶ probing %s · %s @ %s", profile, p.LLM, p.URL)))
		return probeBackend(m.cli, profile, false)
	}
	m.appendLine(styleOK.Render(fmt.Sprintf("✓ active: %s · %s @ %s", profile, p.LLM, p.URL)))
	return pingBackend(m.cli.BaseURL)
}

// rebuildClient swaps in a fresh llm.Client for the now-active profile.
// Replacing the pointer (not mutating fields) drops the prior Client's sticky
// state (noReasoningEffort, keep-alive pool tied to the old URL): new
// endpoint, fresh slate.
func (m *Model) rebuildClient() {
	p := m.cfg.ActiveProfile()
	m.cli = llm.New(m.cfg.ActiveURL(), p.LLM, p.Key)
	// Drop the prior profile's cached BudgetStatus. m.budget has no profile
	// association, so without this reset the footer keeps rendering the old
	// "88% pass" segment after switching to a local profile that emits no
	// X-Budget-* headers (nothing would overwrite it). A fresh BudgetStatus{}
	// hides the segment until the new backend reports its own.
	m.budget = cloud.BudgetStatus{}
}

func (m Model) cmdClear(_ []string) (tea.Model, tea.Cmd) {
	m.history = nil
	m.scroll.Reset()
	m.sessionTokens = 0
	m.streamingEstimate = 0
	// Reset the repeated-failure streak so the next turn starts clean.
	m.failKey, m.failStreak = "", 0
	// Wipe prompt recall too: in-memory ring and on-disk .codehamr/history,
	// or leftover history would contradict the "fresh start" promise.
	m.promptHistory = nil
	m.histIdx = -1
	_ = clearPromptHistory(m.cfg.Dir)
	// Full wipe (unlike Ctrl+L, which redraws but keeps scrollback).
	// tea.ClearScreen emits \x1b[2J, which only wipes the viewport; the
	// saved-lines buffer needs eraseScrollback (DECSED 3) too, or old replies
	// stay scrollable above the reset line. tea.Sequence keeps the print from
	// racing past the clear (tea.Batch runs both concurrently and the print
	// could land first, then get wiped). scroll keeps the line for resize
	// replay; outbox is cleared because the Sequence owns the print now.
	line := styleOK.Render("✓ conversation reset")
	m.scroll.WriteString(line + "\n")
	m.outbox = nil
	return m, tea.Sequence(tea.ClearScreen, eraseScrollback, tea.Println(line))
}

// hamrpassMinKeyLen guards against half-pasted keys: real keys clear 16,
// stray fragments don't.
const hamrpassMinKeyLen = 16

// hamrpassValidate is the single source of truth for whether a key is
// acceptable and what the UI says about it. Shared by the inline /hamrpass
// handler and the arg popover hint. ok=false with an empty trimmed key is the
// "show status block" signal.
//
// Non-printable/non-ASCII runes are rejected up front: http.Header.Set accepts
// the bytes but http.Client.Do then errors with `invalid header field value
// for "Authorization"` on the wire, after the key has already been persisted
// to config.yaml. Real keys are ASCII-printable; reject anything else early.
func hamrpassValidate(raw string) (key, hint string, ok bool) {
	key = strings.TrimSpace(raw)
	switch {
	case key == "":
		return "", "paste your hamrpass key, or Enter for status", false
	case strings.ContainsAny(key, " \t\r\n"):
		return key, "no whitespace allowed", false
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return key, "key must be printable ASCII (no control chars)", false
		}
	}
	if len(key) < hamrpassMinKeyLen {
		return key, fmt.Sprintf("%d/%d chars · keep typing", len(key), hamrpassMinKeyLen), false
	}
	return key, "Enter to activate", true
}

// hamrpassArgHint is the args callback for /hamrpass: one synthetic row whose
// value mirrors the typed argument and whose description carries the live
// validation hint. Mirroring keeps the row alive: refreshSuggest filters via
// HasPrefix(value, prefix), and HasPrefix(x, x) is always true.
func hamrpassArgHint(m Model) []argOption {
	_, rest, _ := strings.Cut(m.ta.Value(), " ")
	rest = strings.TrimLeft(rest, " ")
	_, hint, ok := hamrpassValidate(rest)
	mark := "· "
	switch {
	case ok:
		mark = "✓ "
	case rest != "":
		mark = "✗ "
	}
	return []argOption{{value: rest, description: mark + hint}}
}

// cmdHamrpass: `/hamrpass` shows status + how-to, `/hamrpass <key>` validates,
// saves the key on the managed hamrpass profile, switches active, and pings the
// backend. Validation lives in hamrpassValidate so the popover hint and the
// inline error stay in lockstep.
func (m Model) cmdHamrpass(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.printHamrpassStatus()
		return m, nil
	}
	if len(args) > 1 {
		m.appendLine(styleError.Render("⚠ hamrpass keys cannot contain spaces"))
		return m, nil
	}
	key, hint, ok := hamrpassValidate(args[0])
	if !ok {
		m.appendLine(styleError.Render("⚠ " + hint))
		return m, nil
	}
	return m, m.activateHamrpass(key)
}

// printHamrpassStatus emits the status + how-to block (the no-args path).
func (m *Model) printHamrpassStatus() {
	hp, ok := m.cfg.Models["hamrpass"]
	status := "unset"
	if ok && strings.TrimSpace(hp.Key) != "" {
		status = "set"
	}
	url, llmName := "https://codehamr.com", "hamrpass"
	if ok {
		url, llmName = hp.URL, hp.LLM
	}
	m.appendLine(styleHamr.Render("hamrpass") + styleDim.Render(" · prepaid pass for the hosted codehamr endpoint"))
	m.appendLine(styleDim.Render(fmt.Sprintf("  status   : %s", status)))
	m.appendLine(styleDim.Render(fmt.Sprintf("  endpoint : %s", url)))
	m.appendLine(styleDim.Render(fmt.Sprintf("  llm      : %s", llmName)))
	m.appendLine("")
	m.appendLine("A hamrpass is a prepaid pot of budget for our hosted, agent")
	m.appendLine("tuned model. No subscription, no expiry, no rate limits. The")
	m.appendLine("pass simply runs out when the budget is spent. Top up at")
	m.appendLine("https://codehamr.com.")
	m.appendLine("")
	m.appendLine(styleDim.Render("To activate:"))
	m.appendLine(styleDim.Render("  /hamrpass <your key>            paste here, switches active profile"))
	m.appendLine(styleDim.Render("  or edit .codehamr/config.yaml   set models.hamrpass.key directly"))
	m.appendLine("")
	m.appendLine(styleDim.Render("Once set, the remaining pass percentage appears in the status bar."))
}

// activateHamrpass writes the key onto the hamrpass profile (seeding the entry
// if the user removed it from config.yaml), switches active, rebuilds the
// client, and runs the shared confirmation (probe path, since hamrpass now has
// a key).
func (m *Model) activateHamrpass(key string) tea.Cmd {
	hp := m.cfg.EnsureHamrpass()
	hp.Key = key
	if err := m.cfg.SetActive("hamrpass"); err != nil {
		m.appendLine(styleError.Render("⚠ " + err.Error()))
		return nil
	}
	m.rebuildClient()
	return m.confirmActive("hamrpass")
}
