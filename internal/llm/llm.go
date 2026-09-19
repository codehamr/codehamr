// Package llm is codehamr's only LLM client. It speaks the OpenAI Responses
// wire format and nothing else: one POST to `$BaseURL/v1/responses`, SSE
// streamed back, no per-backend branches.
//
// One code path serves every backend:
//   - OpenAI directly (its current models accept tools only here)
//   - local Ollama (0.13.3+), vLLM and llama.cpp, via the `/v1/responses`
//     endpoint each ships alongside chat completions
//   - the codehamr.com hosted endpoint, hamrpass-keyed
//   - any other endpoint already speaking OpenAI's Responses format
//
// Deliberately unsupported, to keep the client uniform: chat completions,
// Ollama's native `/api/chat`, and every stateful Responses feature
// (`previous_response_id`, conversations). History is replayed in full on every
// request, reasoning items included, so the server needs no memory of us.
//
// If you're special-casing a provider here, the fix almost always belongs on
// the server: make it emit standard OpenAI shapes.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codehamr/codehamr/internal/cloud"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Tool is one function tool as the Responses API declares it: flat, with no
// `function` wrapper.
type Tool struct {
	Type        string         `json:"type"` // always "function"
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// request is the outbound body. Store is serialised even when false (no
// omitempty): stateless mode is what makes the server hand reasoning back
// inline (OpenAI encrypts it) instead of keeping it server-side.
type request struct {
	Model     string     `json:"model"`
	Input     []any      `json:"input"`
	Tools     []Tool     `json:"tools,omitempty"`
	Stream    bool       `json:"stream"`
	Store     bool       `json:"store"`
	Reasoning *reasoning `json:"reasoning,omitempty"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

// Input items, one struct per shape rather than one with omitempty fields:
// OpenAI rejects keys that don't belong to an item's type, and a
// function_call_output must carry `output` even when the tool printed nothing
// (a silent heredoc write), so no field here is optional. Reasoning items
// aren't declared at all: they round-trip as the raw JSON the server emitted.
type messageItem struct {
	Type    string `json:"type"` // "message"
	Role    string `json:"role"`
	Content string `json:"content"`
}

type functionCallItem struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON text, as the server streamed it
}

type functionOutputItem struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// streamEvent is one Responses SSE frame, decoded loosely: every event type
// shares this one shape and readSSE branches on Type. Only fields acted on are
// declared; sequence numbers, content indices and the echoed request are
// ignored. Item stays raw because a reasoning item is replayed verbatim (its
// encrypted payload is opaque to us) and a function_call item is only peeked.
type streamEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	Arguments   string          `json:"arguments"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	// Message is the text of a top-level `error` event.
	Message  string `json:"message"`
	Response *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	// Error is the bare `{"error":{...}}` frame proxies emit when the provider
	// dies after 200 OK, then close without a completion event. Left
	// undecoded, the close would read as a clean EOF.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// outputItem is the slice of an output item the reader acts on.
type outputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Event is what the TUI consumes. One event per stream update.
type Event struct {
	Kind    EventKind
	Content string
	// ContextWindow is the server-authoritative context size from the
	// X-Context-Window header, set only on EventDone when the server sent it.
	// The TUI records it in a runtime-only per-profile map (tui.Model's
	// liveContextSize) that outranks the profile's on-disk ContextSize, so the
	// next ctx.Pack uses what the server allows without the live value ever
	// reaching config.yaml. Zero means no live value in this response.
	ContextWindow int
	ToolCall      *chmctx.ToolCall
	Final         *chmctx.Message
	Budget        cloud.BudgetStatus
	Tokens        int
	// PromptTokens is the server-counted prompt size from usage.prompt_tokens,
	// set only on EventDone when the server reported usage. Debug-log
	// calibration only (actual vs the char/4 packing estimate); it drives no
	// behavior. Zero means the server didn't report it.
	PromptTokens int
	Elapsed      time.Duration
	Err          error
	// MidStream marks an EventError raised after the stream had already
	// delivered at least one frame and that is not the server's own diagnosis:
	// a dropped socket rather than a refusal. History is only written on
	// EventDone, so the TUI can re-issue the identical request without
	// duplicating anything. See Model.replayStream.
	MidStream bool
}

type EventKind int

const (
	EventContent EventKind = iota
	EventToolCall
	EventDone
	EventError
	// EventReasoning carries incremental reasoning text, kept out of history;
	// it exists only so the UI can tick its live token estimate while thinking.
	EventReasoning
	// EventToolArgs carries an incremental tool-call arguments fragment (in
	// Content), the bytes a write_file/edit_file/bash call streams as it's
	// generated. Like EventReasoning it never touches history (the resolved
	// call still arrives whole as EventToolCall at stream end) and exists only
	// so the UI's live token estimate keeps ticking while the model writes a
	// file, instead of freezing until EventDone.
	EventToolArgs
	// EventRetry announces one transparent resend after a transient pre-stream
	// failure (see retryable): Content carries a short status-bar hint
	// ("retry 1/3 in 2s"), Err the failure that triggered it. Purely
	// informational — it never touches history — and exists so the backoff
	// wait doesn't read as a frozen turn.
	EventRetry
)

// streamIdleTimeout bounds how long readSSE waits for the NEXT SSE frame before
// treating the stream as dead. It is an inter-frame (idle) timeout, not an
// end-to-end one: a slow-but-alive stream keeps arriving frames (content,
// reasoning, even blank/keepalive lines), each resetting the watchdog, so only a
// connection gone silent after 200 OK trips it. The silent window that matters is
// the pre-first-token gap: a local model emits nothing while it prefills the
// prompt (or cold-reloads after a keep_alive eviction), and a 27B on modest
// hardware can stay silent well past two minutes there; a 120s value killed such
// live streams mid-prefill. A big-context turn (2nd/3rd prompt on a complex
// codebase) makes prefill scale with packed-history size, and an
// OpenAI-compatible server typically streams nothing during it, so even 600s
// can trip on a still-working model. 1h is the default so a user can walk away;
// erring long is cheap on two counts. A genuinely dead socket is caught far
// sooner by OS TCP keepalive (Go's default Dialer probes the peer), independent
// of this timeout, so a long value means "patient with a live-but-slow stream",
// not "frozen forever on a dead one". And it stays escapable instantly with
// Ctrl+C (request-context cancel unblocks the read), whereas killing a live
// stream loses the turn. This idle timeout is NOT the loop/stuck guard: a
// looping model emits frames and resets the watchdog every time, so it slips
// straight past; runaway/failure nudges and Ctrl+C own that. CODEHAMR_IDLE_TIMEOUT
// overrides the default (Go duration like "90m", or a bare number = seconds).
const streamIdleTimeout = time.Hour

// streamInterFrameTimeout bounds the gap between two SSE frames once the stream
// is live. A server mid-answer emits continuously; minutes of silence there is a
// dead connection, not a slow model. Kept well clear of the prefill window
// above, which is the wait that legitimately runs long.
const streamInterFrameTimeout = 5 * time.Minute

// idleTimeoutFromEnv resolves CODEHAMR_IDLE_TIMEOUT to a duration, falling back
// to streamIdleTimeout when unset or unparseable. Accepts a Go duration string
// ("45m", "1h30m") or a bare number read as seconds. Lives here, not in main's
// applyEnvOverrides, because it's purely an llm concern and both Client call
// sites (startup + /models switch) go through New.
func idleTimeoutFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("CODEHAMR_IDLE_TIMEOUT"))
	if v == "" {
		return streamIdleTimeout
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		// Overflow check: a huge bare-seconds value (e.g. nanoseconds pasted
		// where seconds were meant) can wrap the multiply to a small positive
		// duration, silently killing every live-but-slow stream mid-prefill.
		if d := time.Duration(n) * time.Second; d/time.Second == time.Duration(n) {
			return d
		}
	}
	return streamIdleTimeout
}

// defaultRetryBackoff paces the transparent resends after a transient
// pre-stream failure (see retryable), one wait per retry. Rising steps: short
// enough that a proxy hiccup heals invisibly, and a *permanently* failing
// request (e.g. a genuinely wrong model name 404ing forever) still surfaces
// after ~22s total instead of minutes.
var defaultRetryBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

type Client struct {
	BaseURL string
	Model   string
	Token   string // optional; empty = no Authorization header
	HTTP    *http.Client
	// IdleTimeout caps the wait for the next SSE frame (see streamIdleTimeout).
	// A field, not a bare const, only so tests can shorten it; New sets the
	// default and nothing else writes it.
	IdleTimeout time.Duration
	// RetryBackoff spaces Chat's pre-stream retries on transient failures;
	// len is the retry count, nil/empty disables. A field, not a bare const,
	// only so tests can shorten it; New sets the default and nothing else
	// writes it.
	RetryBackoff []time.Duration
	// noReasoning goes true once the server 400s on the reasoning effort for
	// this model (Ollama on a non-thinking model, vLLM on a value outside the
	// model's scale, OpenAI on a non-reasoning model). Sticky for the Client's
	// lifetime so later turns skip to the supported shape; a `/models` switch
	// builds a fresh Client and resets it, correctly, since the new endpoint
	// may have different rules.
	//
	// atomic.Bool: Probe and Chat race on the same Client (startup probe still
	// in flight when the first turn fires) and both read it via post; Chat may
	// also write it. A plain bool would be a data race.
	noReasoning atomic.Bool
}

// New builds a Client governed by the caller's context, not http.Client.Timeout.
// That timeout is end-to-end (it covers body reads) and would kill a slow but
// legitimate SSE stream mid-flight on slow local backends. tui.Model's turnCtx
// is the single cancellation source; connect-level safety (DNS/TCP) is already
// bounded by Go's default Dialer (30s).
func New(base, model, token string) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(base, "/"),
		Model:        model,
		Token:        token,
		HTTP:         &http.Client{},
		IdleTimeout:  idleTimeoutFromEnv(),
		RetryBackoff: defaultRetryBackoff,
	}
}

// ProbeResult holds what Probe extracts from a one-shot hello request: the
// live context window and budget snapshot. The TUI shows the real window at
// activation time and feeds the authoritative size to ctx.Pack instead of the
// config.yaml fallback.
type ProbeResult struct {
	ContextWindow int
	Budget        cloud.BudgetStatus
}

// Probe sends a minimal hello chat just to harvest response headers in one
// round trip: status validates the URL/model/key combo, X-Context-Window gives
// the live size, X-Budget-Remaining the live fraction. The body is closed
// unread; on the cloud proxy that may already charge one token, the cost of a
// single round-trip "key works AND here is your real window". Returns the
// standard cloud errors (Unreachable, Unauthorized, BudgetExhausted) for
// errors.Is branching.
func (c *Client) Probe(parent context.Context) (ProbeResult, error) {
	resp, budget, err := c.post(parent, request{
		Model:  c.Model,
		Input:  []any{messageItem{Type: "message", Role: "user", Content: "hi"}},
		Stream: true,
	})
	if err != nil {
		return ProbeResult{Budget: budget}, err
	}
	defer resp.Body.Close()
	return ProbeResult{
		ContextWindow: cloud.ContextWindowFromHeaders(resp.Header),
		Budget:        cloud.FromHeaders(resp.Header),
	}, nil
}

// Chat streams an assistant response on the returned channel, closing it when
// the stream ends. Reasoning runs at `medium` effort: decode is the serialised
// critical path of every round, and `high` bought deliberation the agent loop
// already gets from seeing each tool result. If the server rejects the effort
// (see rejectsReasoning), post drops it for this Client's lifetime so the model
// still works at the server's own default.
func (c *Client) Chat(parent context.Context, messages []chmctx.Message, tools []Tool) <-chan Event {
	out := make(chan Event, 32)
	go c.run(parent, messages, tools, out)
	return out
}

func (c *Client) run(parent context.Context, msgs []chmctx.Message, tools []Tool, out chan<- Event) {
	defer close(out)
	start := time.Now()

	// Pre-stream retry: transient failures (proxy hiccups: 5xx, 429, LiteLLM's
	// transient 404) are resent after a rising backoff instead of killing the
	// turn. Only here, before any token has streamed — a mid-stream resend
	// would duplicate content already in the transcript. Probe stays
	// retry-free: its job is fast feedback on a misconfigured profile.
	resp, errEvt := c.sendChat(parent, msgs, tools)
	for attempt := 0; errEvt != nil && attempt < len(c.RetryBackoff) && retryable(errEvt.Err); attempt++ {
		delay := c.RetryBackoff[attempt]
		hint := fmt.Sprintf("retry %d/%d in %s", attempt+1, len(c.RetryBackoff), delay)
		if !sendEvent(parent, out, Event{Kind: EventRetry, Content: hint, Err: errEvt.Err}) {
			return
		}
		select {
		case <-time.After(delay):
		case <-parent.Done():
			// Ctrl+C during the wait: unwind silently, exactly like a
			// cancelled sendEvent — the TUI already aborted the turn.
			return
		}
		resp, errEvt = c.sendChat(parent, msgs, tools)
	}
	if errEvt != nil {
		sendEvent(parent, out, *errEvt)
		return
	}
	defer resp.Body.Close()

	// Idle watchdog: bufio.Scanner.Scan() ignores context, so a server that
	// stops sending after 200 OK would wedge readSSE forever. Closing the body
	// from the timer unblocks the in-flight Read; readSSE then returns and we
	// surface a stall. parent isn't cancelled, so (unlike Ctrl+C) the error
	// reaches the user.
	//
	// TWO windows, not one. The silent pre-first-token prefill scales with the
	// packed context and can legitimately run many minutes, so it gets the long
	// window. Every window after the first covers a LIVE stream, where minutes
	// of silence means a dead socket, so readSSE's onFrame swaps in the short
	// one. Arming both at the prefill length is what let a dropped connection
	// cost a full hour of an unattended run.
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = streamIdleTimeout
	}
	interFrame := min(idle, streamInterFrameTimeout)
	var stalled, streaming atomic.Bool
	window := atomic.Int64{}
	window.Store(int64(idle))
	watchdog := time.AfterFunc(idle, func() {
		stalled.Store(true)
		resp.Body.Close()
	})

	budget := cloud.FromHeaders(resp.Header)
	ctxWindow := cloud.ContextWindowFromHeaders(resp.Header)
	final, tokens, promptTokens, err := readSSE(parent, resp.Body, budget, out, func(output bool) {
		if output {
			streaming.Store(true)
			window.Store(int64(interFrame))
		}
		watchdog.Reset(time.Duration(window.Load()))
	})
	watchdog.Stop()
	if err != nil {
		if stalled.Load() {
			err = fmt.Errorf("the server stopped sending data (no stream activity for %s)", time.Duration(window.Load()))
		}
		// MidStream marks a drop the TUI may transparently replay. Gated on a
		// frame having actually arrived: a PREFILL stall is deterministic, so
		// replaying it just re-pays the same wait. A server-reported stream
		// error is its own diagnosis ("context length exceeded") and would
		// replay forever, so it never qualifies either.
		var sse *serverStreamError
		sendEvent(parent, out, Event{Kind: EventError, Err: err, MidStream: streaming.Load() && !errors.As(err, &sse)})
		return
	}
	sendEvent(parent, out, Event{
		Kind:          EventDone,
		Final:         final,
		Budget:        budget,
		ContextWindow: ctxWindow,
		Tokens:        tokens,
		PromptTokens:  promptTokens,
		Elapsed:       time.Since(start),
	})
}

// sendEvent puts e on out, bailing if parent cancels first, so a slow or
// vanished consumer after Ctrl+C can't wedge the stream goroutine on a full
// buffer.
func sendEvent(parent context.Context, out chan<- Event, e Event) bool {
	select {
	case out <- e:
		return true
	case <-parent.Done():
		return false
	}
}

// sendChat POSTs the request and returns the response on 200. On failure it
// returns the Event the caller forwards, populated with Kind/Err/Budget. The
// body is closed on every non-200 branch; 200 leaves it open for the caller.
func (c *Client) sendChat(parent context.Context, msgs []chmctx.Message, tools []Tool) (*http.Response, *Event) {
	resp, budget, err := c.post(parent, request{
		Model:     c.Model,
		Input:     toInput(msgs),
		Tools:     tools,
		Stream:    true,
		Reasoning: &reasoning{Effort: "medium"},
	})
	if err != nil {
		return nil, &Event{Kind: EventError, Err: err, Budget: budget}
	}
	return resp, nil
}

// post dispatches via doPost; on a 400 rejecting the reasoning effort it drops
// the field for this Client's lifetime and retries once. Probe never sets
// Reasoning, so its 400 can't trip the flag.
func (c *Client) post(parent context.Context, body request) (*http.Response, cloud.BudgetStatus, error) {
	if c.noReasoning.Load() {
		body.Reasoning = nil
	}
	resp, budget, errBody, err := c.doPost(parent, body)
	if err != nil && body.Reasoning != nil && rejectsReasoning(errBody) {
		c.noReasoning.Store(true)
		body.Reasoning = nil
		resp, budget, _, err = c.doPost(parent, body)
	}
	return resp, budget, err
}

// rejectsReasoning reports whether an error body is the server refusing our
// reasoning effort, as opposed to any other 400. Matched on the parameter's
// own name in each dialect (OpenAI "reasoning.effort", vLLM "Unexpected
// reasoning effort", Ollama's "<model> does not support thinking"), never on a
// lone generic word, so an unrelated 400 that merely mentions "thinking" can't
// latch reasoning off for the Client's whole life. Dropping the field is the
// right remedy for all of them, but note what it costs: the server then applies
// its own default (on the Qwen3.8 scale that is xhigh, i.e. MORE reasoning than
// we asked for, not less). A compatibility fix, never a way to think less.
func rejectsReasoning(errBody []byte) bool {
	for _, sig := range []string{"reasoning.effort", "reasoning_effort", "reasoning effort", "does not support thinking"} {
		if bytes.Contains(errBody, []byte(sig)) {
			return true
		}
	}
	return false
}

// doPost performs one round-trip, mapping status into the typed cloud errors
// Probe and sendChat share. On 200 it returns the live response with body open
// for streaming; on non-200 the body is drained and closed first. Budget is set
// only on 402. errBody returns the raw body on a non-2xx other than 401/402, so
// post can check it for the reasoning fallback signal without re-reading.
func (c *Client) doPost(parent context.Context, body request) (*http.Response, cloud.BudgetStatus, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, cloud.BudgetStatus{}, nil, err
	}
	req, err := http.NewRequestWithContext(parent, "POST", c.BaseURL+"/v1/responses", bytes.NewReader(buf))
	if err != nil {
		return nil, cloud.BudgetStatus{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", cloud.AuthHeader(c.Token))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, cloud.BudgetStatus{}, nil, cloud.ErrUnreachable{Err: err}
	}
	if resp.StatusCode == 200 {
		return resp, cloud.BudgetStatus{}, nil, nil
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 401:
		// Drain before the deferred Close so the keep-alive connection returns
		// to the pool instead of being discarded, same as the 402/default arms.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, cloud.BudgetStatus{}, nil, cloud.ErrUnauthorized
	case 402:
		// Pass depleted. Body ignored: the status code is the whole signal,
		// the UI banner is fixed text, the returned snapshot reflects it.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, cloud.BudgetStatus{Set: true, Remaining: 0}, nil, cloud.ErrBudgetExhausted
	default:
		b, _ := io.ReadAll(resp.Body)
		msg := errorMessageFromBody(b)
		if resp.StatusCode == 404 && strings.Contains(msg, "litellm.NotFoundError") {
			msg += " · check LiteLLM's upstream URL and model; if that upstream only supports /chat/completions, set use_chat_completions_api: true under this model's litellm_params in the proxy config"
		} else if resp.StatusCode == 404 {
			// A route miss is the one misconfiguration the body never explains:
			// a server that still only speaks chat completions (Ollama before
			// 0.13.3, an older proxy) says nothing more than "not found". Name
			// the requirement here, where the user reads it. vLLM also 404s an
			// unknown model, so its message leads and the hint follows.
			msg += " · codehamr speaks the OpenAI Responses API (POST /v1/responses); the server needs Ollama 0.13.3+, a current vLLM or llama.cpp, and a model it knows"
		}
		return nil, cloud.BudgetStatus{}, b, &httpStatusError{status: resp.StatusCode, msg: msg}
	}
}

// httpStatusError preserves the HTTP status behind the user-facing message so
// retryable can classify transience; Error() keeps the exact "status: message"
// string the error banner has always shown.
type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("%d: %s", e.status, e.msg) }

// retryable reports whether a pre-stream failure is worth resending: transport
// errors and the statuses that signal a transient server state. 404 is
// semantically permanent, but LiteLLM-style proxies emit it for transient
// upstream misses (the Agnes-AI freeze in issue #7), and the rising backoff
// bounds the cost of a truly permanent one. 401/402 arrive as typed sentinels
// the UI handles; 400 and other client errors fail identically on every
// resend.
func retryable(err error) bool {
	var un cloud.ErrUnreachable
	if errors.As(err, &un) {
		return true
	}
	var hs *httpStatusError
	if errors.As(err, &hs) {
		return hs.status == 404 || hs.status == 408 || hs.status == 429 || hs.status >= 500
	}
	return false
}

// errorMessageFromBody extracts the user-facing string from a non-2xx body.
// hamrpass wraps errors as `{"error":{"message":...,"provider_hint":...}}`; we
// prefer provider_hint (providers stash the human diagnostic there), fall back
// to message, then to the raw first line so non-hamrpass backends still surface
// whatever they emit.
func errorMessageFromBody(b []byte) string {
	var env struct {
		Error struct {
			Message      string `json:"message"`
			ProviderHint string `json:"provider_hint"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil {
		if env.Error.ProviderHint != "" {
			return env.Error.ProviderHint
		}
		if env.Error.Message != "" {
			return env.Error.Message
		}
	}
	return firstLine(string(b))
}

// readSSE reads Responses SSE frames until the completion event or EOF,
// forwarding text and reasoning deltas as events, accumulating tool calls per
// output item, and collecting reasoning items for replay. Returns the final
// assistant message (content + tool calls + reasoning items), the server's
// output and input token counts, and any scanner error. parent is threaded
// through so sends abort on cancellation instead of blocking on an undrained
// buffer.
//
// Frames are decoded loosely and unknown event types are skipped, so
// lifecycle chatter (response.created, content_part.*, reasoning_part.*) and
// a proxy's stray `data: [DONE]` cost nothing. serverStreamError is an error
// the SERVER reported inside the stream, as opposed to a transport failure.
// Typed so the replay path can tell the two apart: resending a request the
// server already refused just repeats the refusal.
type serverStreamError struct{ msg string }

func (e *serverStreamError) Error() string {
	return "the server reported a stream error: " + e.msg
}

func readSSE(parent context.Context, body io.Reader, budget cloud.BudgetStatus, out chan<- Event, onFrame func(output bool)) (*chmctx.Message, int, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<16), 4<<20)

	var (
		content      strings.Builder
		slots        = map[int]*toolSlot{}
		order        []int
		reasoning    []json.RawMessage
		tokens       int
		promptTokens int
		// complete goes true on a terminal response event (completed, or
		// incomplete: the model stopped on purpose, e.g. at max_output_tokens).
		// A stream that ends without one was cut, not finished - a proxy/LB
		// gracefully closing the upstream mid-generation looks exactly like
		// clean EOF to the scanner, and finalizing it would hand the TUI a
		// mid-sentence assistant message as a clean finish: a transport-level
		// false green no nudge can see.
		complete bool
	)
	// Tool calls key on output_index: every fragment of one call carries the
	// same index, and parallel calls get distinct ones. Created on first sight
	// so a server that skips output_item.added still resolves the call.
	slot := func(idx int) *toolSlot {
		t, ok := slots[idx]
		if !ok {
			t = &toolSlot{}
			slots[idx] = t
			order = append(order, idx)
		}
		return t
	}

	for scanner.Scan() {
		// Any line (data, `event:` name, blank separator, or ": keepalive"
		// comment) is liveness and rearms the idle watchdog. Only a `data:`
		// line means the model has actually started PRODUCING, which is the
		// separate question of whether the long prefill window is over: a
		// proxy that emits one comment at 200 OK (LiteLLM/nginx SSE shims,
		// OpenRouter's ": OPENROUTER PROCESSING") must not collapse the
		// prefill window to the inter-frame one, nor make the resulting
		// deterministic prefill stall look like a replayable mid-stream drop.
		line := bytes.TrimSpace(scanner.Bytes())
		isData := bytes.HasPrefix(line, []byte("data:"))
		onFrame(isData)
		if !isData {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			content.WriteString(ev.Delta)
			if !sendEvent(parent, out, Event{Kind: EventContent, Content: ev.Delta, Budget: budget}) {
				return nil, 0, 0, parent.Err()
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			// Forwarded so the UI reflects thinking; never enters content. The
			// reasoning that round-trips is the raw item collected below.
			if !sendEvent(parent, out, Event{Kind: EventReasoning, Content: ev.Delta, Budget: budget}) {
				return nil, 0, 0, parent.Err()
			}
		case "response.output_item.added", "response.output_item.done":
			var item outputItem
			if json.Unmarshal(ev.Item, &item) != nil {
				continue
			}
			switch item.Type {
			case "function_call":
				// added carries call_id and name (arguments still empty); done
				// carries everything, authoritative, for a server that streamed
				// no argument deltas at all.
				slot(ev.OutputIndex).update(item.CallID, item.Name, item.Arguments)
			case "reasoning":
				if ev.Type == "response.output_item.done" {
					reasoning = append(reasoning, ev.Item)
				}
			}
		case "response.function_call_arguments.delta":
			slot(ev.OutputIndex).args.WriteString(ev.Delta)
			// Forward the fragment so the UI's live token estimate ticks while
			// the model streams file content into a tool call: the resolved
			// call still arrives whole as EventToolCall at stream end.
			if !sendEvent(parent, out, Event{Kind: EventToolArgs, Content: ev.Delta, Budget: budget}) {
				return nil, 0, 0, parent.Err()
			}
		case "response.function_call_arguments.done":
			slot(ev.OutputIndex).update("", "", ev.Arguments)
		case "response.completed", "response.incomplete":
			complete = true
			if ev.Response != nil && ev.Response.Usage != nil {
				tokens = ev.Response.Usage.OutputTokens
				promptTokens = ev.Response.Usage.InputTokens
			}
		case "response.failed":
			msg := "response failed"
			if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
				msg = ev.Response.Error.Message
			}
			return nil, 0, 0, &serverStreamError{msg: msg}
		case "error":
			msg := ev.Message
			if msg == "" {
				msg = string(line)
			}
			return nil, 0, 0, &serverStreamError{msg: msg}
		default:
			if ev.Error != nil {
				msg := ev.Error.Message
				if msg == "" {
					msg = string(line)
				}
				return nil, 0, 0, &serverStreamError{msg: msg}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, 0, err
	}
	if !complete {
		// Clean EOF with no completion event: the connection was cut, not
		// finished. Returned as a plain error (not serverStreamError), so run()
		// marks it MidStream when frames had arrived and the TUI's bounded
		// replay re-issues the request instead of finalizing a truncated reply.
		return nil, 0, 0, errors.New("the stream ended without a completion event (response.completed) - the connection was likely cut mid-response")
	}

	// Emit accumulated tool calls once at stream end, in output order. Resolve
	// every slot once, sharing the parsed payload between the events and the
	// final message.
	calls := make([]chmctx.ToolCall, 0, len(order))
	for _, idx := range order {
		calls = append(calls, slots[idx].resolve())
	}
	for i := range calls {
		if !sendEvent(parent, out, Event{Kind: EventToolCall, ToolCall: &calls[i], Budget: budget}) {
			return nil, 0, 0, parent.Err()
		}
	}
	return &chmctx.Message{
		Role:      chmctx.RoleAssistant,
		Content:   content.String(),
		ToolCalls: calls,
		Reasoning: reasoning,
	}, tokens, promptTokens, nil
}

// toolSlot accumulates one streamed tool call. `arguments` arrives as JSON
// fragmented across deltas, each fragment invalid alone; we append raw and
// parse once, in resolve().
type toolSlot struct {
	id, name string
	args     strings.Builder
}

// update takes any non-empty identity field, and REPLACES the arguments when a
// full text is given: arguments.done and output_item.done both carry the whole
// string, so appending would double what the deltas already built.
func (t *toolSlot) update(callID, name, args string) {
	if callID != "" {
		t.id = callID
	}
	if name != "" {
		t.name = name
	}
	if args != "" {
		t.args.Reset()
		t.args.WriteString(args)
	}
}

func (t *toolSlot) resolve() chmctx.ToolCall {
	parsed := map[string]any{}
	if t.args.Len() > 0 {
		if err := json.Unmarshal([]byte(t.args.String()), &parsed); err != nil {
			// Malformed args surface as a sentinel key, not a silently empty
			// map, so the log names what broke. Real args never use
			// _parse_error, so collisions aren't a concern.
			parsed["_parse_error"] = err.Error()
		}
	}
	return chmctx.ToolCall{ID: t.id, Name: t.name, Arguments: parsed}
}

// toInput flattens history into Responses input items. An assistant message
// becomes its reasoning items (verbatim, ahead of what they produced: that is
// the replay contract, and where reasoning continuity across tool rounds comes
// from), then its text, then one function_call per tool call; a tool result
// becomes a function_call_output. Two shapes are deliberately not sent: the
// empty text item beside tool calls (it says nothing), and the reasoning of a
// round that produced neither text nor calls, because a reasoning item with
// nothing following it is the one replay shape OpenAI 400s. Arguments are
// re-marshalled from the parsed map, never raw bytes, so a _parse_error call
// still round-trips as valid JSON instead of poisoning every later request.
func toInput(msgs []chmctx.Message) []any {
	items := make([]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case chmctx.RoleTool:
			items = append(items, functionOutputItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		case chmctx.RoleAssistant:
			if m.Content != "" || len(m.ToolCalls) > 0 {
				for _, r := range m.Reasoning {
					items = append(items, r)
				}
			}
			if m.Content != "" || len(m.ToolCalls) == 0 {
				items = append(items, messageItem{Type: "message", Role: "assistant", Content: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Arguments)
				items = append(items, functionCallItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: string(args)})
			}
		default:
			items = append(items, messageItem{Type: "message", Role: string(m.Role), Content: m.Content})
		}
	}
	return items
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
