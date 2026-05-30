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
	minViewport  = 5               // rows reserved above the prompt for streaming tokens
	popoverCap   = 6               // max rows the popover may claim
	pingTimeout  = 2 * time.Second // backend reachability probe budget
)

// phase is the turn state machine and single source of truth: idle = no turn;
// thinking = awaiting the model, no tokens yet; streaming = content flowing;
// running = a tool is executing.
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
	system  string           // embedded system prompt + working-directory anchor

	// streaming is the live raw token buffer for the current content block,
	// rendered above the prompt by View() while the model talks. On flush
	// (block end, tool call, cancel, error) it's rendered through glamour,
	// queued into outbox for tea.Println, then reset.
	streaming *strings.Builder

	// outbox holds lines bound for terminal scrollback via tea.Println on the
	// next Update cycle. The Update wrapper drains it every cycle, so handlers
	// can call appendLine / flushStreaming without threading a Cmd back.
	outbox []string

	// scroll is the in-memory transcript of every appendLine / flushStreaming
	// line. The real scrollback lives in the user's terminal; this copy is
	// replayed in handleResizeSettle after a width change wipes the terminal,
	// and tests read it to verify what was emitted.
	scroll *strings.Builder

	ta       promptInput
	renderer *glamour.TermRenderer
	spinner  spinner.Model

	// streaming state
	stream <-chan llm.Event

	// pending tool calls waiting to be executed after an assistant turn
	pending []chmctx.ToolCall

	// turn-level stats (reset in finalizeTurn) + session-cumulative count
	// (reset only by /clear, so the status bar carries the running session
	// total).
	turnTokens    int
	turnElapsed   time.Duration
	sessionTokens int
	// streamingEstimate is a live char/4 estimate of tokens for the current
	// round (reasoning + content). The server reports the authoritative count
	// only in the final usage block, so without this the footer would freeze
	// through the whole reasoning phase then jump. Reset to 0 on
	// EventDone/Error, where the real count takes over.
	streamingEstimate int
	budget            cloud.BudgetStatus

	connected bool // last known backend reachability (refreshed on ping / stream error)
	width     int
	height    int

	// View() returns "" while suppressView is on, so bubbletea's async ticker
	// can't commit a stale frame mid-drag.
	suppressView bool
	// resizeGen is bumped per width change; settle ticks act only on the
	// matching gen, so older debounces self-discard.
	resizeGen int

	// splashShown guards first-frame emission; later resizes re-emit via the
	// settle handler.
	splashShown bool

	// arrow-key history: every successful submit is appended; histIdx tracks
	// the ↑/↓ walker position (-1 = current draft, 0 = newest). Entries carry
	// display text and chip state so ↑ reconstructs the original atomic-chip
	// prompt, not just its visible text.
	promptHistory []promptEntry
	histIdx       int

	// slash-autocomplete popover state. suggest holds command rows (when
	// suggestArgLevel is false) or argument rows for activeCmd (when true):
	// same renderer, same keybindings.
	suggest         []argOption
	suggestIdx      int
	suggestOpen     bool
	suggestArgLevel bool
	activeCmd       string

	// per-turn cancel plumbing: one context + CancelFunc govern the LLM stream
	// and tool calls for the turn. Ctrl+C cancels the whole cascade.
	turnCtx     context.Context
	cancel      context.CancelFunc
	quitArmedAt time.Time // first Ctrl+C in idle arms; second within 3s quits

	status string // transient status-bar warning (cleared next render cycle)
	phase  phase  // idle / thinking / streaming / running

	// Repeated-failure nudge — the only deterministic backstop. A turn
	// otherwise ends purely when the model stops calling tools; nothing forces
	// a tool or yields. lastToolKey is the most recently dispatched tool's
	// target identity (set in dispatchNextTool); failKey/failStreak track how
	// often that SAME target failed the SAME way. At maxToolFailStreak we inject
	// one system note to change approach — a nudge, never a hard yield. Keyed on
	// tool+target (not full args) so cosmetic retry differences can't defeat it.
	lastToolKey string
	failKey     string
	failStreak  int

	// liveContextSize is the per-profile, runtime-only context window the
	// server reports via X-Context-Window. Seeded by Probe at activation and
	// refreshed on every chat EventDone, so a server-side change applies on the
	// next prompt without a restart. Authoritative for cloud profiles (whose
	// on-disk ContextSize is intentionally empty); for user-managed profiles
	// it's empty and packing falls back to Profile.ContextSize. Never persisted.
	liveContextSize map[string]int
}

func New(cfg *config.Config, cli *llm.Client, projectDir, version string) Model {
	ta := newPromptInput()

	// Fixed dark style — WithAutoStyle queries the terminal (OSC 11) before
	// bubbletea grabs raw stdin, so the reply bytes leak into the textarea as
	// "1;rgb:1e1e/1e1e/1e1e" garbage. Dev containers are dark: no query, no leak.
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
		// width/height left at 0 — View() returns "" until the first
		// WindowSizeMsg, so we don't flash an 80×24 frame then resize.
		streaming:       new(strings.Builder),
		scroll:          new(strings.Builder),
		histIdx:         -1,
		liveContextSize: map[string]int{},
	}
	// Seed prompt history from .codehamr/history so ↑ recalls prompts from
	// earlier sessions. Loaded entries carry no chip metadata (the on-disk
	// format stores expanded text only), so a recalled multi-line paste
	// appears uncollapsed — the right tradeoff for a cat-friendly history file.
	m.promptHistory = loadPromptHistory(cfg.Dir)
	return m
}

// activeContextSize returns the context window the packer should aim at: the
// live server-reported value for the active profile if known, else the on-disk
// ContextSize, else defaultPackFallback — so cloud profiles before their first
// response (and any missing/zero value) still get a sensible budget.
func (m *Model) activeContextSize() int {
	if v, ok := m.liveContextSize[m.cfg.Active]; ok && v > 0 {
		return v
	}
	if v := m.cfg.ActiveProfile().ContextSize; v > 0 {
		return v
	}
	return defaultPackFallback
}

// defaultPackFallback is the conservative window used until the server reports
// a real value. Matches config.defaultContextSize so cloud profiles behave like
// a fresh local one until X-Context-Window arrives on the next response.
const defaultPackFallback = 131072

// resizeSettleDelay debounces width-resize bursts: longer than typical drag
// SIGWINCH cadence (10–50ms) so a continuous drag collapses to one settle,
// short enough that a one-off resize feels instant.
const resizeSettleDelay = 150 * time.Millisecond

type resizeSettleMsg struct{ gen int }

// eraseScrollback wipes the terminal's saved-lines buffer (DECSED 3); no
// tea.ClearScreen equivalent clears scrollback.
var eraseScrollback tea.Cmd = func() tea.Msg {
	os.Stdout.WriteString(ansi.EraseDisplay(3))
	return nil
}

// pingMsg carries a backend-reachability result. baseURL is the URL probed;
// Update drops the message when it no longer matches the live client's URL,
// else a stale ping from the prior profile (a mid-flight /models switch) would
// overwrite connected state with the wrong endpoint's reachability.
type pingMsg struct {
	ok      bool
	baseURL string
}

// quitArmResetMsg fires ~3s after Ctrl+C arms the quit — if not already quit or
// re-armed, clear the hint from the status bar.
type quitArmResetMsg struct{}

func (m Model) Init() tea.Cmd {
	// Keyed (cloud) profiles get a silent Probe at startup so the status bar
	// renders the live budget / context window from the first frame. Keyless
	// (local Ollama) profiles get the cheaper Reachable ping — no headers to
	// harvest, so a full probe would buy nothing.
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

// Update is the bubbletea entry point: it dispatches to update()'s typed
// handlers then drains the outbox into a single tea.Println, so lines land in
// scrollback in the exact order appendLine / flushStreaming queued them. One
// Println per cycle, never a Batch — Batch runs children concurrently, leaving
// arrival order undefined, so splash lines and tool-call banners would shuffle.
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
		// under tea.WithReportFocus. Swallow them so they never reach
		// textarea.Update — otherwise the escape fragments get parsed as
		// printable runes, inserted into the prompt, and bloat textarea height
		// on every focus switch.
		return m, nil

	case tea.KeyMsg:
		// An empty-runes key can surface when the parser chokes mid-escape.
		// Drop it before recomputeLayout wastes cycles.
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 0 {
			return m, nil
		}
		// Pre-grow the textarea before bubbles processes the key. bubbles'
		// repositionView() runs at the end of textarea.Update and scrolls the
		// viewport down whenever the cursor crosses below current Height, but
		// our recomputeLayout() grows Height only AFTER handleKey returns. So
		// a char that wraps to a new visual row leaves YOffset>0 with the first
		// wrap row clipped off the top, which recomputeLayout can't reclaim.
		// Inflating Height to the screen-cap first keeps the cursor inside the
		// visible band for any normal keystroke, so repositionView doesn't
		// scroll and YOffset stays 0; recomputeLayout then trims Height back to
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
		// Drop stale pings from a prior backend (a /models switch while a ping
		// was in flight). The live client's URL is the source of truth.
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
		// Stale event from a stream the current turn no longer owns (Ctrl+C →
		// fresh submit while the prior readEvent was in flight). Keep draining
		// the channel so the producer goroutine exits cleanly, but never let
		// the event mutate the now-active turn's state.
		if msg.ch != m.stream {
			return m, readEvent(msg.ch)
		}
		return m.handleStream(msg.e)

	case streamClosedMsg:
		// Stale close from the prior turn's channel — running handleStreamClosed
		// would nil out the live m.stream and, worse, finalizeTurn + endTurn the
		// active turn, killing the user's request out from under them.
		if msg.ch != m.stream {
			return m, nil
		}
		return m.handleStreamClosed()

	case toolResultMsg:
		// Stale result from a turn the user already cancelled. Without this
		// drop, the orphan tool message gets appended to the live turn's history
		// (no preceding assistant.tool_calls → the next /v1 request 400s) and
		// startChat would abandon the in-flight stream. The turnCtx tag was
		// captured at runToolCall / syntheticToolResult time; endTurn nils
		// m.turnCtx and a fresh beginTurn installs a new one that can't match.
		if msg.turnCtx != m.turnCtx {
			return m, nil
		}
		dbgWriteMessage("tool_result", msg.Msg)
		m.history = append(m.history, msg.Msg)
		m.recordToolOutcome(msg.Msg.ToolName, msg.Msg.Content)
		// Drain every remaining call before re-entering chat: OpenAI rejects an
		// assistant.tool_calls message followed by fewer tool messages than
		// calls issued, so a partial dispatch 400s and loses the rest.
		// Sequential dispatch in emit order keeps the pairing intact.
		if len(m.pending) > 0 {
			return m.dispatchNextTool()
		}
		// Queue drained — only now is it safe to inject the failure nudge. A
		// system message wedged between assistant.tool_calls and its tool
		// results would break that pairing and 400 the next request.
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

// handleWindowSize tracks new dimensions, rebuilds the glamour renderer on a
// wrap-width change, emits the splash on the first frame, and on a true width
// change starts the debounced resize-settle cycle (suppressView until the
// settle tick lands at the matching gen).
func (m Model) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	first := !m.splashShown
	widthChanged := m.width > 0 && m.width != msg.Width
	m.width, m.height = msg.Width, msg.Height
	m.ta.SetWidth(msg.Width - 2)
	if first || widthChanged {
		// Glamour compiles a stylesheet + template tree per build, so rebuild
		// only on a real wrap-width change — height-only events and intra-drag
		// duplicates reuse the existing renderer.
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
// matching gen. Wipes the terminal (so previous-width rows can't soft-wrap into
// stair-steps), then re-emits splash, replayed scroll, and any pending outbox
// at the new width. Older debounces self-discard on the gen check.
func (m Model) handleResizeSettle(msg resizeSettleMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.resizeGen {
		return m, nil
	}
	m.suppressView = false
	// tea.Sequence keeps order strict — Batch would race the clears with the
	// writes. After the wipe every line below is emitted at the current width,
	// so no previous-width row can soft-wrap into stair-steps.
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

// submit commits a user prompt. sendText is the expanded form sent to the LLM
// (chip labels replaced by their original paste content); echoText is the
// collapsed form shown in scrollback so the chat doesn't swallow 80 lines of
// pasted log every turn; entry is the history snapshot replayed by ↑/↓,
// including chip state.
func (m Model) submit(sendText, echoText string, entry promptEntry) (tea.Model, tea.Cmd) {
	// Redact secret-bearing slash commands (/hamrpass <key>) before they reach
	// any sink that persists or replays them: the scrollback echo (kept in
	// m.scroll, re-emitted verbatim on every resize), the ↑/↓ recall ring, and
	// the on-disk .codehamr/history file. Without this, submit leaks the bearer
	// token into a second on-disk copy and into UI recall, undermining the
	// 0o600 + symlink defences on the key in config.yaml. Raw sendText still
	// flows to runSlash so activation works. No-op for non-secret input.
	safeText := redactSlash(sendText)
	if safeText != sendText {
		echoText = safeText
		entry = promptEntry{display: safeText}
	}
	// Echo to scrollback with the same accent ▌ the textarea uses — one visual
	// language for "your voice" across live input and history.
	m.appendLine(stylePrompt.Render("▌ ") + styleUser.Render(echoText))
	m.promptHistory = append(m.promptHistory, entry)
	m.histIdx = -1
	// Persist the (redacted) prompt so ↑ finds it after a restart. Errors are
	// swallowed: a transient failure isn't worth derailing submit, and a
	// permanent one (read-only .codehamr/) would just be noise on every prompt.
	_ = appendPromptHistory(m.cfg.Dir, safeText)

	if strings.HasPrefix(sendText, "/") {
		dbgWritef("user_slash", "%s", safeText)
		return m.runSlash(sendText)
	}
	// A new user message is a new goal — drop any in-progress failure streak so
	// a stale count can't trip the nudge early. History persists; only the
	// counter resets.
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

// installTurnContext cancels any in-flight turn context and installs a fresh
// per-turn root on m.turnCtx / m.cancel. Cancel-old-then-install-new keeps
// Ctrl+C consistent: one m.cancel() always unwinds the whole current cascade.
func (m *Model) installTurnContext() {
	if m.cancel != nil {
		m.cancel()
	}
	m.turnCtx, m.cancel = context.WithCancel(context.Background())
}

// beginTurn installs a fresh per-turn context, flips phase to thinking, and
// returns the chat stream-reader Cmd. Every path starting a new LLM round
// funnels through here so one m.cancel() cancels the whole cascade.
func (m *Model) beginTurn() tea.Cmd {
	m.installTurnContext()
	m.phase = phaseThinking
	return m.startChat()
}

// appendUserTurn appends a user-role message to history and starts a turn.
// The only path that does so; used by submit.
func (m *Model) appendUserTurn(content string) tea.Cmd {
	m.history = append(m.history, chmctx.Message{Role: chmctx.RoleUser, Content: content})
	return m.beginTurn()
}

// endTurn zeroes per-turn state after a turn finishes or aborts. Pair to
// beginTurn. Cancels the per-turn context unconditionally to release the
// CancelFunc — Background-rooted contexts otherwise leak one child cancelCtx
// per turn until the process exits. Drops pending tool calls so a turn cut
// short mid-dispatch (Ctrl+C or error) can't leak a leftover call into the next
// turn, which would dispatch with stale args and append an orphan tool_result
// whose tool_call_id no longer pairs the latest assistant message. Does NOT
// touch scrollback — callers decide whether to flush streaming or emit a banner.
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
// write_file, edit_file. No loop/control tool — a turn ends when the model
// stops emitting tool calls (see handleStreamClosed).
func (m *Model) buildTools() []llm.Tool {
	return []llm.Tool{
		schemaToTool(tools.BashSchema()),
		schemaToTool(tools.ReadFileSchema()),
		schemaToTool(tools.WriteFileSchema()),
		schemaToTool(tools.EditFileSchema()),
	}
}

// schemaToTool unwraps a tool schema (the map[string]any shape shared by bash
// and the file tools) into the typed llm.Tool the chat payload expects.
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
// re-arms the stream reader; EventError unwinds the turn instead of looping.
// Events arriving after cancellation are drained quietly — acting on them would
// corrupt scroll (EventContent), re-populate pending (EventToolCall), or credit
// a dead turn's tokens (EventDone).
func (m Model) handleStream(e llm.Event) (tea.Model, tea.Cmd) {
	if !m.phase.active() {
		return m, readEvent(m.stream)
	}
	switch e.Kind {
	case llm.EventContent:
		m.applyContent(e)
	case llm.EventReasoning:
		// Reasoning streams while phase stays "thinking": still deliberating,
		// no user-facing content yet. Hidden from the transcript (not written
		// to scroll); only the live token estimate ticks up in the status bar.
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

// applyContent writes one streamed text chunk to the live buffer and promotes
// phase from thinking to streaming on the first chunk so the status bar shows
// tokens flowing. View() renders the buffer live above the prompt; once the
// block ends it's flushed through glamour into scrollback via tea.Println.
func (m *Model) applyContent(e llm.Event) {
	if m.phase == phaseThinking {
		m.phase = phaseStreaming
	}
	m.streaming.WriteString(e.Content)
	m.streamingEstimate += len(e.Content) / 4
}

// applyToolCall queues a streamed tool call for later dispatch. flushStreaming
// up front commits this round's text to scroll before the inline tool-call
// status lands, so the user sees styled text *before* the "▶ bash: ..." line,
// not all at once at turn end.
func (m *Model) applyToolCall(e llm.Event) {
	m.flushStreaming()
	m.pending = append(m.pending, *e.ToolCall)
}

// applyDone closes one LLM round: harvest the live context window, accumulate
// turn/session tokens, append the assistant message, flush streaming. A turn
// with tool calls fires one EventDone per round — counters accumulate so the
// banner reflects the whole turn. Tokens==0 means the backend skipped
// include_usage; the char/4 estimate carries the counter on those servers.
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

// applyError unwinds the turn on a stream error: preserve content streamed
// before the error (so the user keeps failure context), emit the one-line hint,
// drop the pending queue, reset turn state.
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

// abortTurn winds down a turn that did not complete normally: flush in-flight
// text so the partial block lands in scrollback, post the explanatory banner,
// drop pending tool calls, reset per-turn counters and context. Pair to
// applyDone for the happy path.
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

// handleStreamClosed drives what happens after one round's stream finishes:
// dispatch the next pending tool call, or — if none — finalize the turn and
// hand control back. A turn ends precisely when the assistant emits no tool
// calls; there is no loop tool to land on.
func (m Model) handleStreamClosed() (tea.Model, tea.Cmd) {
	m.stream = nil
	// Stale close from a cancelled turn (handleCtrlC / EventError reset phase
	// to idle).
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
// flows through runToolCall — none are special-cased. lastToolKey records this
// call's target so the failure nudge can tell when the model keeps retrying the
// same failing operation (see recordToolOutcome).
func (m Model) dispatchNextTool() (tea.Model, tea.Cmd) {
	call := m.pending[0]
	m.pending = m.pending[1:]
	m.appendLine(styleDim.Render(tools.InlineStatus(call)))
	m.lastToolKey = toolTargetKey(call)
	m.phase = phaseRunning
	return m, runToolCall(m.turnCtx, call)
}

// maxToolFailStreak is how many consecutive same-target failures trigger the
// nudge. Generous on purpose: a model iterating on a hard edit gets several
// attempts before being told it's stuck — catches genuine loops without
// interrupting honest trial-and-error.
const maxToolFailStreak = 5

// toolTargetKey is the stable identity used to detect a repeated-failure loop:
// tool name + its target (the path for file tools, the command's first line for
// bash). Deliberately NOT the full argument set — a full-args key is defeated by
// any cosmetic change between retries (a regenerated file body, a reworded
// command). Keying on the target catches a model hammering the same operation
// while leaving varied exploration alone.
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

// toolResultFailed reports whether a tool result is an error the model should
// react to. File tools wrap errors in parens ("(write error: ...)", "(not
// found: ...)") and report success as plain text ("wrote N bytes"); bash
// appends "(exit: N)" / "(timeout after ...)" on failure. A user Ctrl+C
// ("(cancelled)") never counts as a failure.
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

// recordToolOutcome updates the failure streak from one finished tool result.
// A success (or a failure of a different target) resets the streak; a same-
// target failure extends it. lastToolKey was stamped in dispatchNextTool for
// the call this result belongs to.
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

// maybeFailureNudge appends one system-role note once the same target has
// failed maxToolFailStreak times running, then resets the streak so it fires at
// most once per run of failures. A nudge, not a yield: the model stays in
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

// cursorOnFirstLine: true when ↑ should walk prompt history instead of moving
// the textarea's own cursor. cursorOnLastLine is the mirror for ↓.
func (m Model) cursorOnFirstLine() bool { return m.ta.Line() == 0 }
func (m Model) cursorOnLastLine() bool  { return m.ta.Line() == m.ta.LineCount()-1 }

// buildSystem appends the working-directory anchor to the embedded system
// prompt so "hier" / "here" resolves to a concrete path.
func buildSystem(projectDir string) string {
	return config.DefaultSystemPrompt + "\n\nWorking directory: " + projectDir
}

// pingBackend issues a short GET to baseURL/v1/models via cloud.Reachable. Any
// HTTP response counts as reachable; transport errors and timeouts mean
// disconnected. The result carries the URL it was issued against so Update can
// drop late results arriving after a /models switch.
func pingBackend(baseURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		defer cancel()
		return pingMsg{ok: cloud.Reachable(ctx, baseURL) == nil, baseURL: baseURL}
	}
}
