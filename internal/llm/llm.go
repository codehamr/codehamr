// Package llm is codehamr's only LLM client. It speaks the OpenAI
// chat-completions wire format and nothing else: one POST to
// `$BaseURL/v1/chat/completions`, SSE streamed back, no per-backend branches.
//
// One code path serves every backend:
//   - local Ollama, via the OpenAI-compatible `/v1` shim Ollama itself ships
//   - the codehamr.com hosted endpoint, hamrpass-keyed (proxy over OpenRouter)
//   - any other endpoint already speaking OpenAI's wire format
//
// Deliberately unsupported, to keep the client uniform:
//   - Ollama's native `/api/chat` (NDJSON, different schema, no tool-call IDs)
//   - LiteLLM's `ollama_chat` translator (non-standard deltas, shared indices)
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

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// wireMessage is the outbound OpenAI request shape; responses parse via streamChunk.
//
// Content has no omitempty: silent bash commands (e.g. heredoc writes) yield an
// empty tool-result string, and omitting the field makes Ollama's /v1 shim 400
// with "invalid message content type: <nil>". Always send an explicit string.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`         // tool name
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool role
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	// Index keys which call a streaming delta belongs to. Fragments arrive
	// across chunks; slot lookup MUST key on this, not on slice position.
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // always "function"
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // OpenAI stringifies args
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []wireMessage  `json:"messages"`
	Tools           []Tool         `json:"tools,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

// streamOptions: without include_usage, OpenAI-compatible servers omit the
// usage block in the SSE tail chunk and the per-turn token counter sits at 0.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk is one OpenAI SSE frame. finish_reason is deliberately never
// ROUTED on (readSSE dispatches accumulated tool calls at stream end, not on
// finish_reason=="tool_calls", staying provider agnostic since Ollama's /v1 shim
// sometimes closes with "stop" even after streaming tool_calls); it is decoded
// only as one of the end-of-stream completion signals, where any non-empty
// value - right or wrong - means the server finished on purpose.
type streamChunk struct {
	Choices []struct {
		Delta        streamDelta `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
	} `json:"usage,omitempty"`
	// Error is the mid-stream failure frame OpenAI-compatible backends (and
	// OpenRouter-style proxies) emit when the provider dies after 200 OK:
	// `data: {"error":{...}}`, then the connection closes with no [DONE].
	// Without decoding it, the frame parses to zero choices, the close reads
	// as clean EOF, and a mid-sentence-truncated turn finalizes as a success.
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// Reasoning is the incremental chain-of-thought fragment that reasoning
	// models stream in `delta.reasoning` before answer
	// tokens. Forwarded as EventReasoning to keep the UI animating, but never
	// round-trips into the assistant message: it has no place in history.
	Reasoning string     `json:"reasoning,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
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
	// reasoningFallback is set once the server 400s on reasoning for this
	// model (newer OpenAI models reject tools + reasoning_effort here, pushing
	// that combo onto /v1/responses; Ollama rejects it on non-thinking models).
	// nil until then; afterwards the reasoning_effort value every request
	// sends instead: "" omits the field, "none" sends it explicitly. Sticky for
	// the Client's lifetime so later turns skip to the supported shape; a
	// `/models` switch builds a fresh Client and resets it, correctly, since
	// the new endpoint may have different rules.
	//
	// atomic: Probe and Chat race on the same Client (startup probe still in
	// flight when the first turn fires) and both read it via postChat; Chat
	// may also write it. A plain field would be a data race.
	reasoningFallback atomic.Pointer[string]
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
	resp, budget, err := c.postChat(parent, chatRequest{
		Model:    c.Model,
		Messages: []wireMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
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
// already gets from seeing each tool result. If the server
// rejects the tools + reasoning_effort combo (newer OpenAI models do), postChat
// drops reasoning_effort (or pins it to `none` where the server demands that)
// for this Client's lifetime so the model still works, with tools but no
// reasoning. Staying on chat-completions is the product line;
// we do not branch to /v1/responses to keep reasoning.
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
	resp, budget, err := c.postChat(parent, chatRequest{
		Model:           c.Model,
		Messages:        toWire(msgs),
		Tools:           tools,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		ReasoningEffort: "medium",
	})
	if err != nil {
		return nil, &Event{Kind: EventError, Err: err, Budget: budget}
	}
	return resp, nil
}

// postChat dispatches via doPost; on a 400 rejecting reasoning it swaps
// reasoning_effort for the server's accepted shape for this Client's lifetime
// and retries once. Probe never sets ReasoningEffort, so its 400 can't trip it.
func (c *Client) postChat(parent context.Context, body chatRequest) (*http.Response, cloud.BudgetStatus, error) {
	if fb := c.reasoningFallback.Load(); fb != nil {
		body.ReasoningEffort = *fb
	}
	resp, budget, errBody, err := c.doPost(parent, body)
	if err != nil && body.ReasoningEffort != "" {
		if fb, ok := reasoningFallback(errBody); ok {
			c.reasoningFallback.Store(&fb)
			body.ReasoningEffort = fb
			resp, budget, _, err = c.doPost(parent, body)
		}
	}
	return resp, budget, err
}

// reasoningFallback reports whether an error body is a server refusing our
// reasoning_effort, as opposed to any other 400, and the value to resend with.
// Three wild flavours, all caught by substring match: newer OpenAI models
// ("reasoning_effort … not supported" alongside tools), Ollama non-thinking
// models ("<model> does not support thinking"), and models whose scale simply
// omits our value ("Unexpected reasoning effort high" — Qwen3.8 defines
// xhigh/medium/low, with no `high`). Each signal is the provider's own phrase,
// never a lone generic word, so an unrelated 400 that merely mentions
// "thinking" can't latch reasoning off for the Client's whole life.
//
// The usual remedy is omitting the field (""), but note what it costs: the
// server then applies its own default (on the Qwen3.8 scale that is xhigh,
// i.e. MORE reasoning than we asked for, not less). OpenAI's newest models are
// the exception: their default is a real effort level, so omitting the field
// 400s identically and the message itself demands `'none'`; only then do we
// send it explicitly. A compatibility fix either way, never a way to think less.
func reasoningFallback(errBody []byte) (string, bool) {
	switch {
	case bytes.Contains(errBody, []byte("not support")) &&
		bytes.Contains(errBody, []byte("reasoning_effort")):
		if bytes.Contains(errBody, []byte("'none'")) {
			return "none", true
		}
		return "", true
	case bytes.Contains(errBody, []byte("does not support thinking")):
		return "", true
	case bytes.Contains(errBody, []byte("Unexpected reasoning effort")):
		return "", true
	}
	return "", false
}

// doPost performs one round-trip, mapping status into the typed cloud errors
// Probe and sendChat share. On 200 it returns the live response with body open
// for streaming; on non-200 the body is drained and closed first. Budget is set
// only on 402. errBody returns the raw body on a non-2xx other than 401/402, so
// postChat can check it for the reasoning_effort fallback signal without
// re-reading.
func (c *Client) doPost(parent context.Context, body chatRequest) (*http.Response, cloud.BudgetStatus, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, cloud.BudgetStatus{}, nil, err
	}
	req, err := http.NewRequestWithContext(parent, "POST", c.BaseURL+"/v1/chat/completions", bytes.NewReader(buf))
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
		return nil, cloud.BudgetStatus{}, b, &httpStatusError{status: resp.StatusCode, msg: errorMessageFromBody(b)}
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

// readSSE reads OpenAI SSE frames until [DONE] or EOF, forwarding
// content/reasoning/tool-call events to out. Returns the final assistant
// message (content + accumulated tool calls), the server completion and prompt
// token counts, and any scanner error. parent is threaded through so sends
// abort on cancellation instead of blocking on an undrained buffer.
// serverStreamError is an error the SERVER reported inside the SSE stream, as
// opposed to a transport failure. Typed so the replay path can tell the two
// apart: resending a request the server already refused just repeats the refusal.
type serverStreamError struct{ msg string }

func (e *serverStreamError) Error() string {
	return "the server reported a stream error: " + e.msg
}

func readSSE(parent context.Context, body io.Reader, budget cloud.BudgetStatus, out chan<- Event, onFrame func(output bool)) (*chmctx.Message, int, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<16), 4<<20)

	var (
		fullContent  strings.Builder
		slots        = map[int]*toolSlot{}
		order        []int
		tokens       int
		promptTokens int
		// complete goes true on any end-of-stream signal: [DONE], a non-empty
		// finish_reason, or a usage frame. A stream that ends without one was
		// cut, not finished - a proxy/LB gracefully closing the upstream
		// mid-generation looks exactly like clean EOF to the scanner, and
		// finalizing it would hand the TUI a mid-sentence assistant message as
		// a clean finish: a transport-level false green no nudge can see.
		complete bool
	)

	for scanner.Scan() {
		// Any line (data, blank separator, or ": keepalive" comment) is liveness
		// and rearms the idle watchdog. Only a `data:` line means the model has
		// actually started PRODUCING, which is the separate question of whether
		// the long prefill window is over: a proxy that emits one comment at
		// 200 OK (LiteLLM/nginx SSE shims, OpenRouter's ": OPENROUTER
		// PROCESSING") must not collapse the prefill window to the inter-frame
		// one, nor make the resulting deterministic prefill stall look like a
		// replayable mid-stream drop.
		line := bytes.TrimSpace(scanner.Bytes())
		isData := bytes.HasPrefix(line, []byte("data:"))
		onFrame(isData)
		if len(line) == 0 || !isData {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			complete = true
			break
		}
		var sc streamChunk
		if err := json.Unmarshal(payload, &sc); err != nil {
			continue
		}
		if sc.Error != nil {
			msg := sc.Error.Message
			if msg == "" {
				msg = string(payload)
			}
			return nil, 0, 0, &serverStreamError{msg: msg}
		}
		for _, choice := range sc.Choices {
			if choice.FinishReason != "" {
				complete = true
			}
			if !dispatchDelta(parent, choice.Delta, budget, &fullContent, slots, &order, out) {
				return nil, 0, 0, parent.Err()
			}
		}
		if sc.Usage != nil {
			complete = true
			tokens = sc.Usage.CompletionTokens
			promptTokens = sc.Usage.PromptTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, 0, err
	}
	if !complete {
		// Clean EOF with no completion signal: the connection was cut, not
		// finished. Returned as a plain error (not serverStreamError), so run()
		// marks it MidStream when frames had arrived and the TUI's bounded
		// replay re-issues the request instead of finalizing a truncated reply.
		return nil, 0, 0, errors.New("the stream ended without a completion signal ([DONE], finish_reason, or usage) - the connection was likely cut mid-response")
	}

	// Emit accumulated tool calls once at stream end, independent of
	// finish_reason: Ollama's /v1 shim sometimes closes with "stop" even
	// after streaming tool_calls, so dispatching here (not on
	// finish_reason=="tool_calls") stays provider agnostic. Resolve every slot
	// once, sharing the parsed payload between the events and the final message.
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
		Content:   fullContent.String(),
		ToolCalls: calls,
	}, tokens, promptTokens, nil
}

// dispatchDelta forwards reasoning and content as events, then accumulates
// streamed tool-call fragments into index-keyed slots. Reasoning stays out of
// fullContent (must not round-trip into the assistant message) but is forwarded
// so the UI reflects thinking. Fragments key on the provider's `index`, not
// slice position, since a call's fragments span chunks whose position need not
// match the index. Returns false when parent cancelled mid-send.
func dispatchDelta(parent context.Context, d streamDelta, budget cloud.BudgetStatus, fullContent *strings.Builder, slots map[int]*toolSlot, order *[]int, out chan<- Event) bool {
	if d.Reasoning != "" {
		if !sendEvent(parent, out, Event{Kind: EventReasoning, Content: d.Reasoning, Budget: budget}) {
			return false
		}
	}
	if d.Content != "" {
		fullContent.WriteString(d.Content)
		if !sendEvent(parent, out, Event{Kind: EventContent, Content: d.Content, Budget: budget}) {
			return false
		}
	}
	for _, tc := range d.ToolCalls {
		// Parked slots live at negative keys (below); a wire index is
		// non-negative in any conforming backend, so normalise rather than let
		// a malformed one land on a parked call and corrupt its arguments.
		if tc.Index < 0 {
			tc.Index = 0
		}
		slot, existed := slots[tc.Index]
		// A backend that emits the same index for every call in a parallel batch
		// would otherwise concatenate N argument bodies into one slot and
		// resolve to a single _parse_error. A fragment carrying a DIFFERENT
		// non-empty id than the slot already holds is a new call. Park the
		// finished one under a negative key - wire indices are non-negative, so
		// a parked key can never collide - and let the NEW call keep tc.Index,
		// because the argument fragments that follow carry that same index and
		// no id, and must reach the call currently being streamed. Its place in
		// `order` moves with it, so emission order survives.
		if existed && tc.ID != "" && slot.id != "" && slot.id != tc.ID {
			parked := -len(*order) - 1
			slots[parked] = slot
			for i, k := range *order {
				if k == tc.Index {
					(*order)[i] = parked
					break
				}
			}
			existed = false
		}
		if !existed {
			slot = &toolSlot{}
			slots[tc.Index] = slot
			*order = append(*order, tc.Index)
		}
		// id/name usually arrive in the first fragment, but updating on any
		// non-empty value tolerates a provider that ships them later;
		// otherwise an empty tool_call_id round-trips into history and the
		// next /v1 request 400s on the unpaired tool message.
		if tc.ID != "" {
			slot.id = tc.ID
		}
		if tc.Function.Name != "" {
			slot.name = tc.Function.Name
		}
		slot.args.WriteString(tc.Function.Arguments)
		// Forward the fragment so the UI's live token estimate ticks while the
		// model streams file content into a tool call: the resolved call still
		// arrives whole as EventToolCall at stream end, so this is UI-only.
		if tc.Function.Arguments != "" {
			if !sendEvent(parent, out, Event{Kind: EventToolArgs, Content: tc.Function.Arguments, Budget: budget}) {
				return false
			}
		}
	}
	return true
}

// toolSlot accumulates one streamed tool call. OpenAI delivers `arguments` as
// JSON fragmented across chunks, each fragment invalid alone; we append raw and
// parse once, in resolve().
type toolSlot struct {
	id, name string
	args     strings.Builder
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

func toWire(msgs []chmctx.Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		om := wireMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.ToolName,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			om.ToolCalls = append(om.ToolCalls, toolCall{
				ID:   tc.ID,
				Type: "function",
				Function: toolCallFunc{
					Name:      tc.Name,
					Arguments: string(args),
				},
			})
		}
		out = append(out, om)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
