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
	"github.com/charmbracelet/x/ansi"

	"github.com/codehamr/codehamr/internal/cloud"
	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
)

const (
	defaultWidth = 80              // bootstrap width before the first WindowSizeMsg
	minViewport  = 5               // breathing room reserved above the prompt for streaming tokens
	popoverCap   = 6               // max rows the popover may claim
	pingTimeout  = 2 * time.Second // backend reachability probe budget
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

	// scroll is the in-memory transcript of every line emitted via
	// appendLine / flushStreaming. The actual scrollback lives in the
	// user's terminal; this copy is replayed via tea.Println in
	// handleResizeSettle after a width change wipes the terminal, and
	// tests use it to verify what was emitted.
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
	// total across the whole hamr session).
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

	// View() returns "" while suppressView is on, so the bubbletea
	// renderer's async ticker can't commit a stale frame mid-drag.
	suppressView bool
	// resizeGen is bumped per width change; settle ticks act only on
	// the matching gen so older debounces self-discard.
	resizeGen int

	// splashShown guards the first-frame emission; later resizes
	// re-emit via the settle handler.
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

	// Repeated-failure nudge — the only deterministic backstop that survived
	// GYSD's removal, and the lean replacement for its five Schranken. A turn
	// otherwise ends purely when the model stops calling tools; nothing forces
	// a tool or yields. lastToolKey is the target identity of the most recently
	// dispatched tool (set in dispatchNextTool); failKey/failStreak track how
	// many times in a row that SAME target failed the SAME way. At
	// maxToolFailStreak we inject one system note telling the model to change
	// approach — a nudge, never a hard yield. Keyed on tool+target (not full
	// args) so cosmetic retry differences can't defeat it the way they defeated
	// GYSD's byte-exact S2 guard.
	lastToolKey string
	failKey     string
	failStreak  int

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
const defaultPackFallback = 131072

// resizeSettleDelay debounces width-resize bursts; longer than typical
// drag SIGWINCH cadence (10–50ms) so a continuous drag collapses to one
// settle, short enough that a one-off resize feels instant.
const resizeSettleDelay = 150 * time.Millisecond

type resizeSettleMsg struct{ gen int }

// eraseScrollback wipes the terminal's saved-lines buffer (DECSED 3).
// No tea.ClearScreen equivalent — only this clears scrollback.
var eraseScrollback tea.Cmd = func() tea.Msg {
	os.Stdout.WriteString(ansi.EraseDisplay(3))
	return nil
}

// pingMsg carries the result of a backend-reachability probe. baseURL is
// the URL the probe was issued against; Update drops the message when it
// no longer matches the live client's URL — otherwise a stale ping from
// the prior profile (user /models switched mid-flight) overwrites
// connected state with the wrong endpoint's reachability.
type pingMsg struct {
	ok      bool
	baseURL string
}

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
	if len(nm.outbox) > 0 {
		printCmd := tea.Println(strings.Join(nm.outbox, "\n"))
		nm.outbox = nil
		cmd = tea.Batch(printCmd, cmd)
	}
	return nm, cmd
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
		// Pre-grow the textarea before bubbles processes the key. bubbles'
		// internal repositionView() runs at the end of textarea.Update and
		// scrolls the viewport down whenever the cursor crosses below the
		// current Height — but our recomputeLayout() that grows Height
		// only runs AFTER handleKey returns. Result: typing a char that
		// wraps to a new visual row leaves YOffset>0 with the first wrap
		// row clipped off the top, and growing Height in recomputeLayout
		// can't reclaim those hidden rows. By temporarily inflating Height
		// to the screen-cap before the key reaches the textarea, the
		// cursor stays inside the viewport's visible band for any normal
		// keystroke, repositionView doesn't scroll, and YOffset stays at
		// 0. recomputeLayout below then trims Height back to the actual
		// visualPromptLines so the live region doesn't bloat empty rows.
		m.preGrowTextarea()
		next, cmd := m.handleKey(msg)
		nm := next.(Model)
		nm.recomputeLayout()
		return nm, cmd

	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)

	case resizeSettleMsg:
		return m.handleResizeSettle(msg)

	case pingMsg:
		// Drop stale pings from a prior backend (e.g. user switched /models
		// while a 2s ping was in flight). The live client's URL is the
		// source of truth.
		if msg.baseURL != m.cli.BaseURL {
			return m, nil
		}
		m.connected = msg.ok
		return m, nil

	case probeMsg:
		return m.handleProbe(msg)

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
		// Stale result from a turn the user already cancelled. Without this
		// drop the orphan tool message gets appended to the live turn's
		// history (no preceding assistant.tool_calls means the next /v1
		// request would 400) and startChat would abandon the in-flight
		// stream. The turnCtx tag was captured when runToolCall /
		// syntheticToolResult was created — endTurn nils m.turnCtx, and a
		// fresh beginTurn installs a new one that cannot match the old.
		if msg.turnCtx != m.turnCtx {
			return m, nil
		}
		dbgWriteMessage("tool_result", msg.Msg)
		m.history = append(m.history, msg.Msg)
		m.recordToolOutcome(msg.Msg.ToolName, msg.Msg.Content)
		// When the assistant emitted multiple tool calls in one round, drain
		// the rest before re-entering chat. OpenAI rejects an
		// `assistant.tool_calls` message followed by fewer `tool` messages
		// than calls issued, so a partial dispatch produces a 400 from the
		// server and loses the un-dispatched calls. Sequential dispatch in
		// the order the model emitted them keeps the contract intact.
		if len(m.pending) > 0 {
			return m.dispatchNextTool()
		}
		// Queue drained — safe to inject the repeated-failure nudge now. A
		// system message wedged between an assistant.tool_calls and its tool
		// results would break that pairing and 400 the next request, so it
		// only lands once every result for the round is recorded.
		m.maybeFailureNudge()
		m.phase = phaseThinking
		return m, m.startChat()

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

// handleWindowSize tracks the new dimensions, rebuilds the glamour
// renderer when the wrap width changed, emits the splash on the very first
// frame, and on a true width change starts the debounced resize-settle
// cycle (suppressView until the settle tick lands at the matching gen).
func (m Model) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	first := !m.splashShown
	widthChanged := m.width > 0 && m.width != msg.Width
	m.width, m.height = msg.Width, msg.Height
	m.ta.SetWidth(msg.Width - 2)
	if first || widthChanged {
		// Glamour compiles a syntax stylesheet + template tree per
		// build, so only rebuild when the wrap width actually
		// changes — height-only events and intra-drag duplicates
		// keep reusing the existing renderer.
		if r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(max(msg.Width-4, 1))); err == nil {
			m.renderer = r
		}
	}
	m.recomputeLayout()
	if first {
		m.splashShown = true
		m.outbox = append(m.outbox, m.splashLines()...)
		return m, nil
	}
	if !widthChanged {
		return m, nil
	}
	m.suppressView = true
	m.resizeGen++
	gen := m.resizeGen
	return m, tea.Tick(resizeSettleDelay, func(time.Time) tea.Msg {
		return resizeSettleMsg{gen: gen}
	})
}

// handleResizeSettle fires once the post-resize debounce expires for the
// matching gen. Wipes the terminal (so previous-width rows can't soft-wrap
// into stair-steps), then re-emits splash, replayed scroll, and any pending
// outbox at the new width. Older debounces self-discard on the gen check.
func (m Model) handleResizeSettle(msg resizeSettleMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.resizeGen {
		return m, nil
	}
	m.suppressView = false
	// tea.Sequence keeps order strict — Batch would race the clears
	// with the writes. After the wipe every line below was emitted at
	// the current width, so no previous-width row can soft-wrap into
	// stair-steps.
	cmds := []tea.Cmd{tea.ClearScreen, eraseScrollback}
	if splash := strings.Join(m.splashLines(), "\n"); splash != "" {
		cmds = append(cmds, tea.Println(splash))
	}
	if scroll := strings.TrimRight(m.scroll.String(), "\n"); scroll != "" {
		cmds = append(cmds, tea.Println(scroll))
	}
	if len(m.outbox) > 0 {
		cmds = append(cmds, tea.Println(strings.Join(m.outbox, "\n")))
		m.outbox = nil
	}
	return m, tea.Sequence(cmds...)
}

// submit commits a user prompt. sendText is the expanded form that goes to
// the LLM (chip labels replaced by their original paste content); echoText
// is the collapsed form that appears in scrollback so the chat doesn't
// swallow 80 lines of pasted log every turn; entry is the history snapshot
// replayed by ↑/↓ including chip state.
func (m Model) submit(sendText, echoText string, entry promptEntry) (tea.Model, tea.Cmd) {
	// Redact secret-bearing slash commands (/hamrpass <key>) before they reach
	// any sink that persists or replays them: the scrollback echo (kept in
	// m.scroll and re-emitted verbatim on every resize), the ↑/↓ recall ring,
	// and the on-disk .codehamr/history file. This is the same redactSlash
	// hook that already keeps the key out of the debug log — submit was the
	// one path still leaking the bearer token into a second on-disk copy and
	// into UI recall, undermining the 0o600 + symlink defences guarding the
	// key in config.yaml. The raw sendText still flows to runSlash so
	// activation works. No-op for non-secret input (redactSlash returns its
	// argument unchanged), so normal prompts echo and recall as before.
	safeText := redactSlash(sendText)
	if safeText != sendText {
		echoText = safeText
		entry = promptEntry{display: safeText}
	}
	// Echo the user's line to scrollback with the same accent ▌ that the
	// textarea uses — same visual language for "your voice", across live
	// input and history.
	m.appendLine(stylePrompt.Render("▌ ") + styleUser.Render(echoText))
	m.promptHistory = append(m.promptHistory, entry)
	m.histIdx = -1
	// Persist the (redacted) prompt so ↑ still finds it after a restart.
	// Errors are swallowed: a transient write failure is not worth
	// derailing the user's submit, and a permanent one (e.g. read-only
	// .codehamr/) will keep failing on every prompt — surfacing that here
	// would just be noise.
	_ = appendPromptHistory(m.cfg.Dir, safeText)

	if strings.HasPrefix(sendText, "/") {
		dbgWritef("user_slash", "%s", safeText)
		return m.runSlash(sendText)
	}
	// A new user message is a new goal — drop any in-progress repeated-
	// failure streak so a stale count from the previous goal can't trip the
	// nudge early. History persists; only this counter resets.
	m.failKey, m.failStreak = "", 0
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
// fresh per-turn root on m.turnCtx / m.cancel. The cancel-old-then-
// install-new pattern keeps Ctrl+C semantics consistent: one m.cancel()
// call always unwinds the whole current cascade.
func (m *Model) installTurnContext() {
	if m.cancel != nil {
		m.cancel()
	}
	m.turnCtx, m.cancel = context.WithCancel(context.Background())
}

// beginTurn installs a fresh per-turn context, flips phase to thinking, and
// returns the chat stream-reader Cmd. Every path that starts a new LLM round
// funnels through here so Ctrl+C cancels the whole cascade in one m.cancel()
// call.
func (m *Model) beginTurn() tea.Cmd {
	m.installTurnContext()
	m.phase = phaseThinking
	return m.startChat()
}

// appendUserTurn appends a user-role message to history and starts a turn.
// Used by submit and the GYSD S4 nudge that continues the conversation
// rather than reset it.
func (m *Model) appendUserTurn(content string) tea.Cmd {
	m.history = append(m.history, chmctx.Message{Role: chmctx.RoleUser, Content: content})
	return m.beginTurn()
}

// endTurn zeroes the per-turn state after a turn finishes or is aborted.
// Pair to beginTurn. Cancels the per-turn context unconditionally so the
// CancelFunc is released — Background-rooted contexts otherwise leak the
// child cancelCtx until the process exits, one per turn over a long
// session. Drops any pending tool calls so a yield/end-loop that fires
// before the queue drained ([ask, bash] etc.) cannot leak the leftover
// call into the next turn — that would dispatch with stale args and
// append an orphan tool_result whose tool_call_id no longer pairs with
// the latest assistant message. Does NOT touch scrollback — callers
// decide whether a cancelled turn still needs to flush streaming or
// emit a banner.
func (m *Model) endTurn() {
	if m.cancel != nil {
		m.cancel()
	}
	m.phase = phaseIdle
	m.cancel = nil
	m.turnCtx = nil
	m.pending = nil
}

func (m *Model) buildMessages() []chmctx.Message {
	r := chmctx.Pack(m.history, chmctx.Budget(m.activeContextSize()))
	out := make([]chmctx.Message, 0, len(r.Messages)+1)
	out = append(out, chmctx.Message{Role: chmctx.RoleSystem, Content: m.system})
	return append(out, r.Messages...)
}

// buildTools exposes the four local tools every turn: bash, read_file,
// write_file, edit_file. There is no loop/control tool — a turn ends simply
// when the model stops emitting tool calls (see handleStreamClosed).
func (m *Model) buildTools() []llm.Tool {
	return []llm.Tool{
		schemaToTool(tools.BashSchema()),
		schemaToTool(tools.ReadFileSchema()),
		schemaToTool(tools.WriteFileSchema()),
		schemaToTool(tools.EditFileSchema()),
	}
}

// schemaToTool unwraps a tool schema (the map[string]any shape shared by
// bash + GYSD loop tools) into the typed llm.Tool the chat payload expects.
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
// one round. If tool calls are pending, dispatch the next one. Otherwise the
// model has nothing more to do — finalize the turn and hand control back to
// the user. A turn ends precisely when the assistant emits no tool calls;
// there is no loop tool to land on and nothing to enforce.
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
	m.endTurn()
	return m, nil
}

// dispatchNextTool pops the next pending tool call and runs it. Every tool
// (bash, read_file, write_file, edit_file) flows through runToolCall — there
// are no special-cased tools. lastToolKey records this call's target so the
// repeated-failure nudge can tell when the model keeps retrying the same
// failing operation (see recordToolOutcome).
func (m Model) dispatchNextTool() (tea.Model, tea.Cmd) {
	call := m.pending[0]
	m.pending = m.pending[1:]
	m.appendLine(styleDim.Render(tools.InlineStatus(call)))
	m.lastToolKey = toolTargetKey(call)
	m.phase = phaseRunning
	return m, runToolCall(m.turnCtx, call)
}

// maxToolFailStreak is how many consecutive same-target failures trigger the
// nudge. Five is generous: a model legitimately iterating on a hard edit gets
// several attempts before the orchestrator points out it's stuck — tuned to
// catch genuine loops without interrupting honest trial-and-error.
const maxToolFailStreak = 5

// toolTargetKey is the stable identity used to detect a repeated-failure loop:
// tool name + its target (the path for the file tools, the first line of the
// command for bash). Deliberately NOT the full argument set — GYSD's old S2
// guard keyed on byte-exact args and so was defeated by any cosmetic change
// between retries (a regenerated file body, a reworded command). Keying on the
// target catches a model hammering the same operation while leaving varied
// exploration alone.
func toolTargetKey(call chmctx.ToolCall) string {
	switch call.Name {
	case tools.WriteFileName, tools.EditFileName, tools.ReadFileName:
		path, _ := call.Arguments["path"].(string)
		return call.Name + "|" + path
	case tools.BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		if i := strings.IndexByte(cmd, '\n'); i >= 0 {
			cmd = cmd[:i]
		}
		return call.Name + "|" + strings.TrimSpace(cmd)
	}
	return call.Name
}

// toolResultFailed reports whether a tool result string is an error the model
// should react to. The file tools wrap every error in parens ("(write error:
// ...)", "(not found: ...)") and report success as plain text ("wrote N
// bytes"); bash appends "(exit: N)" / "(timeout after ...)" on failure. A user
// Ctrl+C ("(cancelled)") is never counted as a failure.
func toolResultFailed(name, result string) bool {
	if strings.Contains(result, "(cancelled)") {
		return false
	}
	switch name {
	case tools.WriteFileName, tools.EditFileName, tools.ReadFileName:
		return strings.HasPrefix(strings.TrimSpace(result), "(")
	case tools.BashName:
		return strings.Contains(result, "\n(exit: ") || strings.Contains(result, "(timeout after ")
	}
	return false
}

// recordToolOutcome updates the repeated-failure streak from one finished tool
// result. A success (or a failure of a different target than last time) resets
// the streak; a failure of the same target extends it. lastToolKey was stamped
// in dispatchNextTool for the call this result belongs to.
func (m *Model) recordToolOutcome(name, content string) {
	if !toolResultFailed(name, content) {
		m.failKey, m.failStreak = "", 0
		return
	}
	if m.lastToolKey == m.failKey && m.failKey != "" {
		m.failStreak++
		return
	}
	m.failKey = m.lastToolKey
	m.failStreak = 1
}

// maybeFailureNudge appends one system-role note when the same target has
// failed maxToolFailStreak times in a row, then resets the streak so it fires
// at most once per run of failures. A nudge, not a yield: the model stays in
// control and decides whether to pivot or stop and tell the user.
func (m *Model) maybeFailureNudge() {
	if m.failStreak < maxToolFailStreak {
		return
	}
	m.history = append(m.history, chmctx.Message{
		Role: chmctx.RoleSystem,
		Content: fmt.Sprintf(
			"Note: the last %d tool calls to the same target failed the same way. Stop repeating it — read the error, change your approach, or tell the user what's blocking you.",
			m.failStreak),
	})
	m.failKey, m.failStreak = "", 0
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

// pingBackend issues a short GET to baseURL/v1/models via cloud.Reachable.
// Any HTTP response counts as reachable; transport errors and timeouts mean
// disconnected. The result carries the URL it was issued against so Update
// can drop late results that arrive after a /models switch.
func pingBackend(baseURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		defer cancel()
		return pingMsg{ok: cloud.Reachable(ctx, baseURL) == nil, baseURL: baseURL}
	}
}
