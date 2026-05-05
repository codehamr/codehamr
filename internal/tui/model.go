package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"

	"github.com/codehamr/codehamr/internal/cloud"
	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/gysd"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
	"github.com/codehamr/codehamr/internal/update"
)

// Defaults used before the terminal reports its size via WindowSizeMsg.
// Chosen to keep the first frame legible rather than a collapsed 0×0.
const (
	defaultWidth  = 80
	defaultHeight = 24
	minViewport   = 5 // breathing room reserved above the prompt for streaming tokens
	popoverCap    = 6 // max rows the popover may claim
	pingTimeout   = 2 * time.Second
)

// phase is the turn state machine. Idle = no turn; thinking = waiting on the
// model but no tokens yet; streaming = content is flowing; running = a tool is
// executing. Single source of truth — no parallel `waiting` bool.
type phase int

const (
	phaseIdle phase = iota
	phaseThinking
	phaseStreaming
	phaseRunning
)

func (p phase) active() bool { return p != phaseIdle }

func (p phase) label() string {
	switch p {
	case phaseThinking:
		return "thinking"
	case phaseStreaming:
		return "generating"
	case phaseRunning:
		return "running"
	}
	return ""
}

type Model struct {
	Version    string
	ProjectDir string

	cfg *config.Config
	cli *llm.Client

	history []chmctx.Message // full conversation minus the system prompt
	system  string           // embedded SYS prompt + working-directory anchor

	// streaming is the live raw token buffer for the current content block;
	// rendered above the prompt by View() while the model is talking. On
	// flush (block end, tool call, cancel, error) it is rendered through
	// glamour and queued into outbox for tea.Println, then reset.
	streaming *strings.Builder

	// outbox holds lines to be pushed into terminal scrollback via
	// tea.Println on the next Update cycle. The Update wrapper drains it
	// every cycle, so any handler can call appendLine / flushStreaming
	// without threading a Cmd back manually.
	outbox []string

	// scroll is a passive write-only transcript of every line emitted via
	// appendLine / flushStreaming. Never rendered — the actual scrollback
	// lives in the user's terminal. Kept solely so tests and the optional
	// debug log can verify what was emitted.
	scroll *strings.Builder

	ta       promptInput
	renderer *glamour.TermRenderer
	spinner  spinner.Model

	// streaming state
	stream <-chan llm.Event

	// pending tool calls waiting to be executed after an assistant turn
	pending []chmctx.ToolCall

	// turn-level stats (reset in finalizeTurn) + session-cumulative token
	// count (reset only by /clear, so the status bar carries the running
	// total across the whole hamr session). Tool-call accounting lives on
	// gysd.Session — this struct only carries display stats.
	turnTokens    int
	turnElapsed   time.Duration
	sessionTokens int
	// streamingEstimate is a live char/4 estimate of tokens generated
	// within the current round (reasoning + content). Needed because the
	// server only reports the authoritative count in the final usage
	// block, so without an estimate the footer would sit still for the
	// entire reasoning phase and then jump at the end. Reset to 0 on
	// EventDone/Error — the real count from the server takes over via
	// sessionTokens.
	streamingEstimate int
	budget            cloud.BudgetStatus

	connected bool // last known backend reachability (refreshed on ping / stream error)
	width     int
	height    int

	// splashShown is flipped on the first WindowSizeMsg so the splash is
	// printed exactly once into terminal scrollback at startup. From then
	// on the splash scrolls up naturally as the conversation grows — no
	// in-app "splash hides on first content" branch to maintain.
	splashShown bool

	// arrow-key history: every successful submit is appended; histIdx tracks
	// where the ↑/↓ walker currently is (-1 = current draft, 0 = newest).
	// Entries carry both display text and chip state so ↑ reconstructs the
	// original atomic-chip prompt, not just its visible text.
	promptHistory []promptEntry
	histIdx       int

	// slash-autocomplete popover state. `suggest` holds either command rows
	// (when suggestArgLevel is false) or argument rows for activeCmd (when
	// true). Same renderer, same keybindings — one source of truth.
	suggest         []argOption
	suggestIdx      int
	suggestOpen     bool
	suggestArgLevel bool
	activeCmd       string

	// per-turn cancel plumbing: one context and one CancelFunc govern the
	// LLM stream and tool calls for the duration of a turn. Ctrl+C cancels
	// the whole cascade.
	turnCtx     context.Context
	cancel      context.CancelFunc
	quitArmedAt time.Time // first Ctrl+C in idle arms; second within 3s quits

	status string // transient status-bar warning (cleared next render cycle)
	phase  phase  // idle / thinking / streaming / running

	// gysd holds the loop state machine: VerifyLog (evidence pool for
	// `done`), recent-call ring (S2), red-streak / missing-loop-tool
	// counters, and the deterministic Schranken from data/gysd.md. One
	// instance per Model; the package never spawns goroutines so all
	// mutations stay on the UI thread.
	gysd *gysd.Session

	// updateAvailable is flipped by the passive startup probe in
	// internal/update. It drives a single dim line in renderSplash; once the
	// user types anything the splash vanishes and the hint with it. Never
	// blocks startup or interrupts a turn.
	updateAvailable bool

	// liveContextSize is the per-profile, runtime-only context window
	// reported by the server via X-Context-Window. Populated by Probe at
	// activation and refreshed on every chat EventDone, so a server-side
	// change picks up on the next prompt without a restart. The map is
	// the authoritative source for cloud profiles (whose on-disk
	// ContextSize is intentionally empty); for user-managed profiles it
	// is empty and packing falls back to Profile.ContextSize. Never
	// persisted — config.yaml stays clean.
	liveContextSize map[string]int
}

func New(cfg *config.Config, cli *llm.Client, projectDir, version string) Model {
	ta := newPromptInput()

	// Fixed dark style — WithAutoStyle queries the terminal (OSC 11) before
	// bubbletea takes raw control of stdin, so the reply bytes leak into the
	// textarea as "1;rgb:1e1e/1e1e/1e1e" garbage. Dev containers are dark-
	// themed; no query, no leak.
	r, _ := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(defaultWidth-4))

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = styleSpinner

	m := Model{
		Version:    version,
		ProjectDir: projectDir,
		cfg:        cfg,
		cli:        cli,
		system:     buildSystem(projectDir),
		ta:         ta,
		renderer:   r,
		spinner:    sp,
		connected:  true, // optimistic until the first ping proves otherwise
		// width/height intentionally left at 0 — View() returns "" until the
		// first WindowSizeMsg arrives, so we don't flash an 80×24 frame and
		// then resize.
		streaming:       new(strings.Builder),
		scroll:          new(strings.Builder),
		histIdx:         -1,
		liveContextSize: map[string]int{},
		gysd:            &gysd.Session{},
	}
	// Seed prompt history from .codehamr/history so ↑ recalls prompts the
	// user typed in earlier sessions of this project. Loaded entries carry
	// no chip metadata (the on-disk format stores expanded text only); a
	// recalled multi-line paste therefore appears uncollapsed, which is
	// the right tradeoff for a dumb cat-friendly history file.
	m.promptHistory = loadPromptHistory(cfg.Dir)
	return m
}

// activeContextSize returns the context window the packer should aim at:
// the live server-reported value if known for the active profile, the
// on-disk ContextSize otherwise, with a safe floor of defaultPackFallback
// so cloud profiles before their first response (or any profile with a
// missing/zero value) still produce a sensible budget.
func (m *Model) activeContextSize() int {
	if v, ok := m.liveContextSize[m.cfg.Active]; ok && v > 0 {
		return v
	}
	if v := m.cfg.ActiveProfile().ContextSize; v > 0 {
		return v
	}
	return defaultPackFallback
}

// defaultPackFallback is the conservative window used until the server
// reports a real value. Matches config.defaultContextSize so cloud
// profiles behave like a fresh local profile until X-Context-Window
// arrives — which is the very next response.
const defaultPackFallback = 65536

// pingMsg carries the result of a backend-reachability probe.
type pingMsg struct{ ok bool }

// updateAvailableMsg fires only when the passive update probe confirms the
// running binary differs from the latest release's published sha256. No
// negative counterpart: a silent check must stay silent if there's nothing
// to say, so the renderer doesn't have to undo a false positive.
type updateAvailableMsg struct{}

// quitArmResetMsg fires ~3s after Ctrl+C arms the quit — if we haven't
// already been quit or re-armed, clear the hint from the status bar.
type quitArmResetMsg struct{}

func (m Model) Init() tea.Cmd {
	// Profiles with a key (cloud endpoints) get a silent Probe at startup so
	// the status bar can render the live budget / context window from the
	// very first frame instead of waiting for the user's first turn. Keyless
	// profiles (local Ollama) get the cheaper Reachable ping — they have no
	// headers to harvest, so the round trip would buy nothing.
	connectivity := pingBackend(m.cli.BaseURL)
	if p := m.cfg.ActiveProfile(); p != nil && p.Key != "" {
		connectivity = probeBackend(m.cli, m.cfg.Active, true)
	}
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
		connectivity,
		checkUpdate(),
	)
}

// Update is the bubbletea entry point. It dispatches to the typed handlers
// in update() and then drains the outbox into a single tea.Println so the
// lines land in scrollback in the exact order appendLine / flushStreaming
// queued them. One Println per cycle, never a Batch of Println Cmds:
// tea.Batch runs its children concurrently and the printLineMessage
// arrival order would be undefined — splash lines and tool-call banners
// would shuffle visibly.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm := next.(Model)
	if len(nm.outbox) == 0 {
		return nm, cmd
	}
	printCmd := tea.Println(strings.Join(nm.outbox, "\n"))
	nm.outbox = nil
	if cmd == nil {
		return nm, printCmd
	}
	return nm, tea.Batch(printCmd, cmd)
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.FocusMsg, tea.BlurMsg:
		// Terminal focus reports (CSI I / CSI O) arrive as these typed msgs
		// when tea.WithReportFocus is on. Swallow them outright so they
		// never leak to textarea.Update — otherwise the escape fragments
		// were getting parsed as printable runes, inserted into the prompt,
		// and bloating the textarea height on every focus switch.
		return m, nil

	case tea.KeyMsg:
		// Defensive: an empty-runes key can surface when the parser chokes
		// mid-escape-sequence. Drop it before recomputeLayout wastes cycles.
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 0 {
			return m, nil
		}
		next, cmd := m.handleKey(msg)
		nm := next.(Model)
		nm.recomputeLayout()
		return nm, cmd

	case tea.WindowSizeMsg:
		first := !m.splashShown
		m.width, m.height = msg.Width, msg.Height
		m.ta.SetWidth(msg.Width - 2)
		// Glamour wraps a few cells inside the terminal width so long
		// lines have breathing room when they land in scrollback.
		if r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(max(msg.Width-4, 1))); err == nil {
			m.renderer = r
		}
		m.recomputeLayout()
		if first {
			// Print the splash exactly once, on the first size message,
			// so it lands in scrollback above the live prompt and scrolls
			// away naturally as content arrives.
			m.splashShown = true
			for _, line := range m.splashLines() {
				m.outbox = append(m.outbox, line)
			}
		}
		return m, nil

	case pingMsg:
		m.connected = msg.ok
		return m, nil

	case probeMsg:
		return m.handleProbe(msg)

	case updateAvailableMsg:
		m.updateAvailable = true
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case streamEventMsg:
		// Stale event from a stream the current turn no longer owns
		// (Ctrl+C → fresh submit while the prior readEvent Cmd was still
		// in flight). Keep draining the same channel so the producer
		// goroutine can exit cleanly, but never let the event mutate
		// state belonging to whatever turn is now active.
		if msg.ch != m.stream {
			return m, readEvent(msg.ch)
		}
		return m.handleStream(msg.e)

	case streamClosedMsg:
		// Stale close from the prior turn's channel — the new turn's
		// m.stream is some other channel and it must keep running. If
		// we ran handleStreamClosed here we would nil out m.stream and,
		// worse, finalizeTurn + endTurn against the live turn, killing
		// the user's request out from under them.
		if msg.ch != m.stream {
			return m, nil
		}
		return m.handleStreamClosed()

	case toolResultMsg:
		dbgWriteMessage("tool_result", msg.Msg)
		m.history = append(m.history, msg.Msg)
		m.phase = phaseThinking
		return m, m.startChat()

	case verifyResultMsg:
		// applyVerifyResult guards against stale msgs by checking
		// phase.active() before mutating gysd state — same drop-on-
		// inactive contract as toolResultMsg.
		return m.applyVerifyResult(msg)

	case quitArmResetMsg:
		if !m.quitArmedAt.IsZero() && time.Now().After(m.quitArmedAt) {
			m.quitArmedAt = time.Time{}
			if m.status == quitArmText {
				m.status = ""
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.recomputeLayout()
	return m, cmd
}

// submit commits a user prompt. sendText is the expanded form that goes to
// the LLM (chip labels replaced by their original paste content); echoText
// is the collapsed form that appears in scrollback so the chat doesn't
// swallow 80 lines of pasted log every turn; entry is the history snapshot
// replayed by ↑/↓ including chip state.
func (m Model) submit(sendText, echoText string, entry promptEntry) (tea.Model, tea.Cmd) {
	// Echo the user's line to scrollback with the same accent ▌ that the
	// textarea uses — same visual language for "your voice", across live
	// input and history.
	m.appendLine(stylePrompt.Render("▌ ") + styleUser.Render(echoText))
	m.promptHistory = append(m.promptHistory, entry)
	m.histIdx = -1
	// Persist the expanded prompt so ↑ still finds it after a restart.
	// Errors are swallowed: a transient write failure is not worth
	// derailing the user's submit, and a permanent one (e.g. read-only
	// .codehamr/) will keep failing on every prompt — surfacing that here
	// would just be noise.
	_ = appendPromptHistory(m.cfg.Dir, sendText)

	if strings.HasPrefix(sendText, "/") {
		dbgWritef("user_slash", "%s", sendText)
		return m.runSlash(sendText)
	}
	// Every user message starts a fresh GYSD sub-loop: the previous
	// VerifyLog, RedStreak, RecentCalls, and missing-loop-tool streak
	// don't carry into the next goal. The model's history persists
	// separately — only the orchestrator's evidence pool is wiped.
	m.gysd.AfterUserMessage()
	dbgWritef("user", "%s", sendText)
	return m, m.appendUserTurn(sendText)
}

func (m *Model) startChat() tea.Cmd {
	msgs := m.buildMessages()
	ch := m.cli.Chat(m.turnCtx, msgs, m.buildTools())
	m.stream = ch
	return readEvent(ch)
}

// installTurnContext cancels any in-flight turn context and installs a
// fresh per-turn root. Returns the new context for callers that need to
// thread it into background work directly; m.turnCtx and m.cancel hold
// the same pair. The cancel-old-then-install-new pattern keeps Ctrl+C
// semantics consistent: one m.cancel() call always unwinds the whole
// current cascade, whether it was started by submit or a plan nudge.
func (m *Model) installTurnContext() context.Context {
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCtx = ctx
	m.cancel = cancel
	return ctx
}

// beginTurn installs a fresh per-turn context, flips phase to thinking,
// resets the per-turn loop-tool flag, and returns the chat stream-reader
// Cmd. Every path that starts a new LLM round (user submit, S4 nudge for
// missing loop tool) funnels through here so Ctrl+C cancels the whole
// cascade in one m.cancel() call.
func (m *Model) beginTurn() tea.Cmd {
	m.installTurnContext()
	m.gysd.BeginTurn()
	m.phase = phaseThinking
	return m.startChat()
}

// appendUserTurn appends a user-role message to history and starts a turn.
// Used by submit and the plan-mode nudges that continue the conversation
// rather than reset it.
func (m *Model) appendUserTurn(content string) tea.Cmd {
	m.history = append(m.history, chmctx.Message{Role: chmctx.RoleUser, Content: content})
	return m.beginTurn()
}

// endTurn zeroes the per-turn state after a turn finishes or is aborted.
// Pair to beginTurn. Cancels the per-turn context unconditionally so the
// CancelFunc is released — Background-rooted contexts otherwise leak the
// child cancelCtx until the process exits, one per turn over a long
// session. Does NOT touch pending or scrollback — callers decide whether a
// cancelled turn still needs to flush streaming or emit a banner.
func (m *Model) endTurn() {
	if m.cancel != nil {
		m.cancel()
	}
	m.phase = phaseIdle
	m.cancel = nil
	m.turnCtx = nil
}

func (m *Model) buildMessages() []chmctx.Message {
	r := chmctx.Pack(m.history, chmctx.Budget(m.activeContextSize()))
	if banner := chmctx.DropBanner(r); banner != "" {
		m.appendLine(styleWarn.Render(banner))
	}
	out := make([]chmctx.Message, 0, len(r.Messages)+1)
	out = append(out, chmctx.Message{Role: chmctx.RoleSystem, Content: m.system})
	return append(out, r.Messages...)
}

func (m *Model) buildTools() []llm.Tool {
	out := []llm.Tool{}
	out = append(out, schemaToTool(tools.BashSchema()))
	out = append(out, schemaToTool(tools.WriteFileSchema()))
	// GYSD loop tools (verify/done/ask) are always exposed alongside
	// bash/write_file — one mode, no phase gating, no triage. The
	// orchestrator enforces "every turn ends with one of these three"
	// via the gysd Session, not via tool-availability tricks.
	for _, s := range gysd.LoopTools() {
		out = append(out, schemaToTool(s))
	}
	return out
}

// schemaToTool unwraps a tool schema (the map[string]any shape shared by
// bash + plan tools) into the typed llm.Tool the chat payload expects.
func schemaToTool(s map[string]any) llm.Tool {
	fn := s["function"].(map[string]any)
	return llm.Tool{
		Type: s["type"].(string),
		Function: llm.FunctionDef{
			Name:        fn["name"].(string),
			Description: fn["description"].(string),
			Parameters:  fn["parameters"].(map[string]any),
		},
	}
}

// handleStream dispatches one llm.Event to the matching apply* helper and
// re-arms the stream reader. EventError unwinds the turn instead of looping.
// Events arriving after the turn was cancelled are drained quietly: acting on
// them would corrupt scroll (EventContent), re-populate pending (EventToolCall),
// or credit a dead turn's tokens (EventDone).
func (m Model) handleStream(e llm.Event) (tea.Model, tea.Cmd) {
	if !m.phase.active() {
		return m, readEvent(m.stream)
	}
	switch e.Kind {
	case llm.EventContent:
		m.applyContent(e)
	case llm.EventReasoning:
		// Reasoning streams while phase stays "thinking" — model is still
		// deliberating, no user-facing content yet. Not written to scroll
		// (reasoning is hidden from the transcript); only the live token
		// estimate ticks up in the status bar.
		m.streamingEstimate += len(e.Content) / 4
	case llm.EventToolCall:
		m.applyToolCall(e)
	case llm.EventDone:
		m.applyDone(e)
	case llm.EventError:
		return m, m.applyError(e)
	}
	return m, readEvent(m.stream)
}

// applyContent writes one streamed text chunk to the live buffer and
// promotes the phase from thinking to streaming on the first chunk so the
// status bar reflects that tokens are flowing. The streaming buffer is
// rendered live by View() above the prompt; once the block ends it is
// flushed through glamour into terminal scrollback via tea.Println.
func (m *Model) applyContent(e llm.Event) {
	if m.phase == phaseThinking {
		m.phase = phaseStreaming
	}
	m.streaming.WriteString(e.Content)
	m.streamingEstimate += len(e.Content) / 4
}

// applyToolCall queues a streamed tool call for later dispatch. The
// flushStreaming up front commits any text streamed in this round to scroll
// before the inline tool-call status lands, so the user sees styled text
// *before* the "▶ bash: ..." line, not all at once at turn end.
func (m *Model) applyToolCall(e llm.Event) {
	m.flushStreaming()
	m.pending = append(m.pending, *e.ToolCall)
}

// applyDone closes one LLM round: harvest the live context window, accumulate
// turn/session tokens, append the assistant message, and flush streaming. A
// turn with tool calls produces one EventDone per round — counters accumulate
// across rounds so the banner reflects the whole turn, not just the last
// round. Tokens==0 means the backend skipped include_usage; the char/4
// estimate carries the counter on those servers.
func (m *Model) applyDone(e llm.Event) {
	m.budget = e.Budget
	if e.ContextWindow > 0 {
		m.liveContextSize[m.cfg.Active] = e.ContextWindow
	}
	delta := e.Tokens
	if delta == 0 {
		delta = m.streamingEstimate
	}
	m.turnTokens += delta
	m.turnElapsed += e.Elapsed
	m.sessionTokens += delta
	m.streamingEstimate = 0
	m.connected = true
	if e.Final != nil {
		dbgWriteMessage("assistant", *e.Final)
		m.history = append(m.history, *e.Final)
	}
	m.flushStreaming()
}

// applyError unwinds the current turn on a stream error: preserve any
// content streamed before the error (so the user keeps failure context),
// emit the one-line hint, drop the pending queue, and reset turn state.
func (m *Model) applyError(e llm.Event) tea.Cmd {
	dbgWritef("error", "%v", e.Err)
	if isUnreachable(e.Err) {
		m.connected = false
	}
	if errors.Is(e.Err, cloud.ErrBudgetExhausted) {
		m.budget = e.Budget
	}
	m.abortTurn(styleError.Render(m.errorMessage(e)))
	return nil
}

// abortTurn winds down a turn that did not complete normally: flush any
// in-flight streamed text so the partial block lands in scrollback, post
// the banner explaining what happened, drop pending tool calls, and reset
// per-turn counters and context. Pair to applyDone for the happy path.
func (m *Model) abortTurn(banner string) {
	m.flushStreaming()
	m.streamingEstimate = 0
	if banner != "" {
		m.appendLine(banner)
	}
	m.pending = nil
	m.finalizeTurn()
	m.endTurn()
}

func (m *Model) finalizeTurn() {
	if m.turnTokens == 0 && m.turnElapsed == 0 {
		return
	}
	banner := fmt.Sprintf("%s · %s", humanTokens(m.turnTokens), humanDuration(m.turnElapsed))
	if rate := humanRate(m.turnTokens, m.turnElapsed); rate != "" {
		banner += " · " + rate
	}
	m.appendLine(styleStatus.Render(banner))
	m.turnTokens = 0
	m.turnElapsed = 0
}

// handleStreamClosed drives what happens after the LLM stream finishes for
// one round. If tool calls are pending, dispatch the next one. Otherwise
// finalize the turn, run the GYSD loop-conformity check (S4/S5), and
// either nudge for verify/done/ask, yield to the user, or end normally.
func (m Model) handleStreamClosed() (tea.Model, tea.Cmd) {
	m.stream = nil
	// Stale close from a turn the user already cancelled (handleCtrlC /
	// EventError paths reset phase to idle).
	if !m.phase.active() {
		return m, nil
	}
	if len(m.pending) > 0 {
		return m.dispatchNextTool()
	}
	m.finalizeTurn()
	// GYSD S4/S5: a turn that didn't end with verify/done/ask gets a
	// nudge appended as a user-turn (re-enters chat with one more
	// chance), or yields hard once the streak hits MaxMissingStreak.
	r := m.gysd.EnsureLoopTool()
	switch {
	case r.Yield:
		m.appendLine(styleWarn.Render(r.UserBlock))
		m.endTurn()
		return m, nil
	case r.ToolPayload != "":
		return m, m.appendUserTurn(r.ToolPayload)
	}
	m.endTurn()
	return m, nil
}

// dispatchNextTool pops the next pending tool call and routes it. GYSD
// loop tools (verify/done/ask) short-circuit through gysd.Session;
// everything else (bash, write_file) flows through runToolCall. S2
// (identical-call repeat detector) is enforced here before any dispatch —
// 3rd identical call (name + canonical args) in the last MaxRecentCalls
// aborts the whole pending queue and yields.
func (m Model) dispatchNextTool() (tea.Model, tea.Cmd) {
	call := m.pending[0]
	m.pending = m.pending[1:]
	if r := m.gysd.NoteToolCall(call.Name, call.Arguments); r.Yield {
		m.abortTurn(styleWarn.Render(r.UserBlock))
		return m, nil
	}
	m.appendLine(styleDim.Render(tools.InlineStatus(call)))
	if gysd.IsLoopTool(call.Name) {
		return m.handleGYSDTool(call)
	}
	m.phase = phaseRunning
	return m, runToolCall(m.turnCtx, call)
}

// cursorOnFirstLine: true when ↑ should walk the prompt history instead of
// moving the textarea's own cursor. cursorOnLastLine is the mirror for ↓.
func (m Model) cursorOnFirstLine() bool { return m.ta.Line() == 0 }
func (m Model) cursorOnLastLine() bool  { return m.ta.Line() == m.ta.LineCount()-1 }

// buildSystem appends the working-directory anchor to the embedded system
// prompt so "hier" / "here" always resolves to a concrete path.
func buildSystem(projectDir string) string {
	return config.DefaultSystemPrompt + "\n\nWorking directory: " + projectDir
}

// pingBackend issues a short GET to the backend root via cloud.Reachable.
// Any HTTP response counts as reachable; transport errors and timeouts mean
// disconnected.
func pingBackend(baseURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		defer cancel()
		return pingMsg{ok: cloud.Reachable(ctx, baseURL) == nil}
	}
}

// checkUpdate fires a single passive update probe during Init. It resolves
// the running executable, hashes it, and compares against the latest
// release's published sha256. A positive result emits updateAvailableMsg so
// renderSplash can surface a dim hint; negative or errored results emit
// nothing (returning nil from a tea.Cmd is a no-op dispatch). The whole
// thing is bounded by update.fetchTimeout so it can never delay startup.
func checkUpdate() tea.Cmd {
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if update.Check(ctx, exe) {
			return updateAvailableMsg{}
		}
		return nil
	}
}
