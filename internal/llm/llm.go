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
	"fmt"
	"io"
	"net/http"
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

// streamChunk is one OpenAI SSE frame. finish_reason is deliberately not
// decoded: readSSE dispatches accumulated tool calls at stream end, not on
// finish_reason=="tool_calls", staying provider agnostic since Ollama's /v1 shim
// sometimes closes with "stop" even after streaming tool_calls.
type streamChunk struct {
	Choices []struct {
		Delta streamDelta `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
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
	// The TUI overwrites the profile's in-memory ContextSize so the next
	// ctx.Pack uses what the server allows, not config.yaml's guess. Zero
	// means no live value in this response.
	ContextWindow int
	ToolCall      *chmctx.ToolCall
	Final         *chmctx.Message
	Budget        cloud.BudgetStatus
	Tokens        int
	Elapsed       time.Duration
	Err           error
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
)

// streamIdleTimeout bounds how long readSSE waits for the NEXT SSE frame before
// treating the stream as dead. It is an inter-frame (idle) timeout, not an
// end-to-end one: a slow-but-alive stream keeps arriving frames (content,
// reasoning, even blank/keepalive lines), each resetting the watchdog, so only a
// connection gone silent after 200 OK trips it. The silent window that matters is
// the pre-first-token gap: a local model emits nothing while it prefills the
// prompt (or cold-reloads after a keep_alive eviction), and a 27B on modest
// hardware can stay silent well past two minutes there; a 120s value killed such
// live streams mid-prefill. 600s clears that worst case; erring long is cheap
// because a genuinely dead socket is still escapable instantly with Ctrl+C
// (request-context cancel unblocks the read), whereas killing a live stream loses
// the turn.
const streamIdleTimeout = 600 * time.Second

type Client struct {
	BaseURL string
	Model   string
	Token   string // optional; empty = no Authorization header
	HTTP    *http.Client
	// IdleTimeout caps the wait for the next SSE frame (see streamIdleTimeout).
	// A field, not a bare const, only so tests can shorten it; New sets the
	// default and nothing else writes it.
	IdleTimeout time.Duration
	// noReasoningEffort goes true once the server 400s on reasoning for this
	// model (newer OpenAI models reject tools + reasoning_effort here, pushing
	// that combo onto /v1/responses; Ollama rejects it on non-thinking models).
	// Sticky for the Client's lifetime so later turns skip
	// to the supported shape; a `/models` switch builds a fresh Client and
	// resets it, correctly, since the new endpoint may have different rules.
	//
	// atomic.Bool: Probe and Chat race on the same Client (startup probe still
	// in flight when the first turn fires) and both read it via postChat; Chat
	// may also write it. A plain bool would be a data race.
	noReasoningEffort atomic.Bool
}

// New builds a Client governed by the caller's context, not http.Client.Timeout.
// That timeout is end-to-end (it covers body reads) and would kill a slow but
// legitimate SSE stream mid-flight on slow local backends. tui.Model's turnCtx
// is the single cancellation source; connect-level safety (DNS/TCP) is already
// bounded by Go's default Dialer (30s).
func New(base, model, token string) *Client {
	return &Client{
		BaseURL:     strings.TrimRight(base, "/"),
		Model:       model,
		Token:       token,
		HTTP:        &http.Client{},
		IdleTimeout: streamIdleTimeout,
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
// the stream ends. Reasoning runs at `high` effort by default; if the server
// rejects the tools + reasoning_effort combo (newer OpenAI models do), postChat
// drops reasoning_effort for this Client's lifetime so the model still works,
// with tools but no reasoning. Staying on chat-completions is the product line;
// we do not branch to /v1/responses to keep reasoning.
func (c *Client) Chat(parent context.Context, messages []chmctx.Message, tools []Tool) <-chan Event {
	out := make(chan Event, 32)
	go c.run(parent, messages, tools, out)
	return out
}

func (c *Client) run(parent context.Context, msgs []chmctx.Message, tools []Tool, out chan<- Event) {
	defer close(out)
	start := time.Now()

	resp, errEvt := c.sendChat(parent, msgs, tools)
	if errEvt != nil {
		sendEvent(parent, out, *errEvt)
		return
	}
	defer resp.Body.Close()

	// Idle watchdog: bufio.Scanner.Scan() ignores context, so a server that
	// stops sending after 200 OK would wedge readSSE forever. Closing the body
	// from the timer unblocks the in-flight Read; readSSE then returns and we
	// surface a stall. parent isn't cancelled, so (unlike Ctrl+C) the error
	// reaches the user. readSSE resets the timer on every frame.
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = streamIdleTimeout
	}
	var stalled atomic.Bool
	watchdog := time.AfterFunc(idle, func() {
		stalled.Store(true)
		resp.Body.Close()
	})

	budget := cloud.FromHeaders(resp.Header)
	ctxWindow := cloud.ContextWindowFromHeaders(resp.Header)
	final, tokens, err := readSSE(parent, resp.Body, budget, out, func() { watchdog.Reset(idle) })
	watchdog.Stop()
	if err != nil {
		if stalled.Load() {
			err = fmt.Errorf("the server stopped sending data (no stream activity for %s)", idle)
		}
		sendEvent(parent, out, Event{Kind: EventError, Err: err})
		return
	}
	sendEvent(parent, out, Event{
		Kind:          EventDone,
		Final:         final,
		Budget:        budget,
		ContextWindow: ctxWindow,
		Tokens:        tokens,
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
		ReasoningEffort: "high",
	})
	if err != nil {
		return nil, &Event{Kind: EventError, Err: err, Budget: budget}
	}
	return resp, nil
}

// postChat dispatches via doPost; on a 400 rejecting reasoning it drops
// reasoning_effort for this Client's lifetime and retries once. Two wild
// flavours, both caught by substring match: newer OpenAI models
// ("reasoning_effort … not supported") and Ollama non-thinking models
// ("<model> does not support thinking"). Each signal is the provider's own
// phrase ("not support"+"reasoning_effort", or the literal "does not support
// thinking") so an unrelated 400 that merely contains the word "thinking"
// can't trip the fallback and latch reasoning off for the Client's whole life.
// Probe never sets ReasoningEffort, so its 400 can't trip the flag.
func (c *Client) postChat(parent context.Context, body chatRequest) (*http.Response, cloud.BudgetStatus, error) {
	if c.noReasoningEffort.Load() {
		body.ReasoningEffort = ""
	}
	resp, budget, errBody, err := c.doPost(parent, body)
	if err != nil && body.ReasoningEffort != "" &&
		((bytes.Contains(errBody, []byte("not support")) &&
			bytes.Contains(errBody, []byte("reasoning_effort"))) ||
			bytes.Contains(errBody, []byte("does not support thinking"))) {
		c.noReasoningEffort.Store(true)
		body.ReasoningEffort = ""
		resp, budget, _, err = c.doPost(parent, body)
	}
	return resp, budget, err
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
		return nil, cloud.BudgetStatus{}, b, fmt.Errorf("%d: %s", resp.StatusCode, errorMessageFromBody(b))
	}
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
// message (content + accumulated tool calls), the server token count, and any
// scanner error. parent is threaded through so sends abort on cancellation
// instead of blocking on an undrained buffer.
func readSSE(parent context.Context, body io.Reader, budget cloud.BudgetStatus, out chan<- Event, onFrame func()) (*chmctx.Message, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<16), 4<<20)

	var (
		fullContent strings.Builder
		slots       = map[int]*toolSlot{}
		order       []int
		tokens      int
	)

	for scanner.Scan() {
		// Any line (data, blank separator, or ": keepalive" comment) is
		// liveness; reset the idle watchdog before inspecting it.
		onFrame()
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var sc streamChunk
		if err := json.Unmarshal(payload, &sc); err != nil {
			continue
		}
		for _, choice := range sc.Choices {
			if !dispatchDelta(parent, choice.Delta, budget, &fullContent, slots, &order, out) {
				return nil, 0, parent.Err()
			}
		}
		if sc.Usage != nil {
			tokens = sc.Usage.CompletionTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
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
			return nil, 0, parent.Err()
		}
	}
	return &chmctx.Message{
		Role:      chmctx.RoleAssistant,
		Content:   fullContent.String(),
		ToolCalls: calls,
	}, tokens, nil
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
		slot, existed := slots[tc.Index]
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
