package tui

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
)

// argOption is one entry in the popover — used both at command-level (one row
// per available command) and at argument-level (one row per accepted value
// for the active command).
type argOption struct {
	value       string // what gets inserted / committed
	description string // right-aligned help text
	current     bool   // rendered bold; default-selected when the popover opens
}

// command is one row in the command-level popover, --help, and the dispatch
// table. `args`, if non-nil, supplies the argument-level popover entries.
// `keepOpen` marks toggle-style commands (e.g. /plugins) whose arg-level
// Enter should re-run the handler in place instead of closing the popover.
type command struct {
	name        string
	description string
	handler     func(Model, []string) (tea.Model, tea.Cmd)
	args        func(Model) []argOption
	keepOpen    bool
}

// commands lists every slash command. Order is the order shown in the popover
// and --help. Keep it short — YAGNI applies to command surface.
var commands = []command{
	{
		name:        "/hamrpass",
		description: "set or show hamrpass key",
		handler:     (Model).cmdHamrpass,
		// args turns the popover into a live key-entry hint: picking
		// /hamrpass auto-inserts the trailing space (handleEnter +
		// handleTab already do this whenever args != nil), then the
		// arg popover renders one synthetic row whose description
		// validates the typed/pasted key live. The row's value mirrors
		// the input so the popover's HasPrefix filter always keeps it,
		// and Enter on the row submits "/hamrpass <key>" via the same
		// path /hamrpass typed manually would take.
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
	{
		name:        "/plugins",
		description: "manage MCP servers",
		handler:     (Model).cmdPlugin,
		keepOpen:    true,
		args: func(m Model) []argOption {
			names := m.cfg.MCPServerNames()
			out := make([]argOption, 0, len(names))
			for _, n := range names {
				s := m.cfg.MCPServers[n]
				mark := "disabled"
				if s.Enabled {
					mark = "enabled"
				}
				out = append(out, argOption{
					value:       n,
					description: mark + " · " + s.Description,
				})
			}
			return out
		},
	},
}

// commandByName returns the registered command with the given slash name,
// or nil when the name is not registered. Popover completion, Enter
// dispatch, refreshSuggest, and runSlash all need the same linear scan —
// this centralises it.
func commandByName(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// runSlash dispatches a slash-prefixed submission. Unknown commands produce a
// quiet hint, not an error.
func (m Model) runSlash(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	if c := commandByName(fields[0]); c != nil {
		return c.handler(m, fields[1:])
	}
	m.appendLine(styleWarn.Render("unknown command — type / to see options"))
	return m, nil
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

// cmdModel: `/models` lists, `/models <name>` sets. Cycling happens in the
// popover via Tab / Shift+Tab — no separate "next" command.
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
// Mirror of printPluginList — same shape, same vocabulary, same call site
// pattern from the no-args branch of the slash handler.
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

// confirmActive emits the activation line for the currently active profile
// and returns the right reachability cmd. Profiles with a key (cloud
// endpoints) get the Probe path: the success line is delayed until the
// hello-world response arrives so it can carry the live ctx window from
// X-Context-Window. Keyless profiles (local Ollama) get the cheaper ping
// and the line prints synchronously. Shared between /models and /hamrpass
// so both paths render the same confirmation.
func (m *Model) confirmActive(profile string) tea.Cmd {
	p := m.cfg.ActiveProfile()
	if p.Key != "" {
		m.appendLine(styleDim.Render(fmt.Sprintf("▶ probing %s · %s @ %s", profile, p.LLM, p.URL)))
		return probeBackend(m.cli, profile, false)
	}
	m.appendLine(styleOK.Render(fmt.Sprintf("✓ active: %s · %s @ %s", profile, p.LLM, p.URL)))
	return pingBackend(m.cli.BaseURL)
}

func (m *Model) rebuildClient() {
	p := m.cfg.ActiveProfile()
	m.cli.BaseURL = strings.TrimRight(m.cfg.ActiveURL(), "/")
	m.cli.Token = p.Key
	m.cli.Model = p.LLM
}

func (m Model) cmdPlugin(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.printPluginList()
		return m, nil
	}
	return m.togglePlugin(args[0]), nil
}

// printPluginList writes the "[✓] name  description" rollup to scroll.
func (m *Model) printPluginList() {
	m.appendLine(styleDim.Render("plugins (/plugins <name> toggles):"))
	for _, name := range m.cfg.MCPServerNames() {
		s := m.cfg.MCPServers[name]
		mark := "[ ]"
		if s.Enabled {
			mark = "[✓]"
		}
		m.appendLine(fmt.Sprintf("  %s %s  %s", mark, name, styleDim.Render(s.Description)))
	}
}

// togglePlugin flips Enabled on the named MCP server, persists, and starts
// or stops the child process to match. Errors fall through to the scroll
// banner so a failed save / spawn never silently rolls back the user's
// intent.
func (m Model) togglePlugin(name string) Model {
	s, ok := m.cfg.MCPServers[name]
	if !ok {
		m.appendLine(styleWarn.Render("unknown plugin: " + name))
		return m
	}
	s.Enabled = !s.Enabled
	m.cfg.MCPServers[name] = s
	if err := m.cfg.Save(); err != nil {
		m.appendLine(styleError.Render("⚠ save: " + err.Error()))
		return m
	}
	if s.Enabled {
		if err := m.mcp.Spawn(name, s); err != nil {
			m.appendLine(styleError.Render("⚠ spawn: " + err.Error()))
			return m
		}
		m.appendLine(styleOK.Render("✓ " + name + " enabled"))
	} else {
		m.mcp.Stop(name)
		m.appendLine(styleOK.Render("✓ " + name + " disabled"))
	}
	return m
}

func (m Model) cmdClear(_ []string) (tea.Model, tea.Cmd) {
	m.history = nil
	m.scroll.Reset()
	m.sessionTokens = 0
	m.streamingEstimate = 0
	// /clear is the full-reset button: plan-executor state goes with it so
	// the next turn starts from a blank slate with no stale phase or task
	// bookkeeping.
	m.resetPlanState()
	// Wipe prompt recall too — both the in-memory ring and the on-disk
	// .codehamr/history. /clear is the project-scoped nuclear option, and
	// leaving prompt history behind would contradict the "fresh start"
	// promise the user gets from the rest of this handler.
	m.promptHistory = nil
	m.histIdx = -1
	_ = clearPromptHistory(m.cfg.Dir)
	// tea.ClearScreen wipes the terminal scrollback so the conversation
	// reset matches what the user sees — without it, /clear would only
	// drop in-memory state while old replies still scrolled above. Pair
	// to Ctrl+L (which also redraws but keeps scrollback): /clear means
	// "fresh start", the screen should reflect that.
	m.appendLine(styleOK.Render("✓ conversation reset"))
	return m, tea.ClearScreen
}

// hamrpassMinKeyLen guards against half-pasted keys. 16 is short enough that
// any real hamrpass key clears the bar and long enough that a typo or stray
// fragment never sneaks through validation.
const hamrpassMinKeyLen = 16

// hamrpassValidate is the single source of truth for "is this key acceptable
// and what should the UI say about it". Two callers share it: the inline
// /hamrpass <key> handler and the arg popover hint. ok=false with an empty
// trimmed key is the "show status block" signal — the caller decides
// whether to print the help screen or simply keep the user typing.
func hamrpassValidate(raw string) (key, hint string, ok bool) {
	key = strings.TrimSpace(raw)
	switch {
	case key == "":
		return "", "paste your hamrpass key, or Enter for status", false
	case strings.ContainsAny(key, " \t\r\n"):
		return key, "no whitespace allowed", false
	case len(key) < hamrpassMinKeyLen:
		return key, fmt.Sprintf("%d/%d chars · keep typing", len(key), hamrpassMinKeyLen), false
	}
	return key, "Enter to activate", true
}

// hamrpassArgHint is the args callback for /hamrpass. It returns one
// synthetic row whose value mirrors the user's currently typed argument and
// whose description carries the live validation hint. Mirroring the value
// is what keeps the row alive across keystrokes — popover.refreshSuggest
// filters via HasPrefix(option.value, argPrefix) and HasPrefix(x, x) is
// always true, so this row never disappears.
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

// cmdHamrpass: `/hamrpass` shows status + how-to, `/hamrpass <key>` validates
// the key, saves it on the managed `hamrpass` profile, switches active to
// hamrpass, and pings the backend so the next render reflects reachability.
// Validation lives in hamrpassValidate so the popover hint and the inline
// error line stay in lockstep.
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

// printHamrpassStatus emits the status + how-to block. Extracted from
// cmdHamrpass so the no-args path stays readable next to the activation
// switch above it.
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

// activateHamrpass writes the key onto the managed hamrpass profile,
// switches active, rebuilds the llm client, and triggers the shared
// activation confirmation (probe path, since hamrpass always has a key
// after this point). Pulled out of cmdHamrpass so the validation switch
// up top reads as a clean gate, with side effects below.
func (m *Model) activateHamrpass(key string) tea.Cmd {
	hp, ok := m.cfg.Models["hamrpass"]
	if !ok {
		// Bootstrap guarantees hamrpass exists; this is a defensive
		// path for hand-edited configs that nuked the entry between
		// Bootstrap and now.
		m.appendLine(styleError.Render("⚠ hamrpass profile missing — restart codehamr to restore it"))
		return nil
	}
	hp.Key = key
	if err := m.cfg.SetActive("hamrpass"); err != nil {
		m.appendLine(styleError.Render("⚠ " + err.Error()))
		return nil
	}
	m.rebuildClient()
	return m.confirmActive("hamrpass")
}
