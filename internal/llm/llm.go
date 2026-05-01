// Package llm is codehamr's only LLM client. It speaks the OpenAI
// chat-completions wire format and nothing else: one POST to
// `$BaseURL/v1/chat/completions`, SSE streaming back, no per-backend branches.
//
// The same code path serves every backend codehamr supports:
//   - local Ollama, via the OpenAI-compatible `/v1` shim Ollama itself ships
//   - the codehamr.com hosted endpoint authenticated by hamrpass keys
//     (FastAPI proxy in front of OpenRouter)
//   - any other endpoint that already speaks OpenAI's wire format
//
// What is *not* supported, on purpose, to keep the client uniform:
//   - Ollama's native `/api/chat` (NDJSON, different schema, no tool-call IDs)
//   - LiteLLM's `ollama_chat` translator (non-standard delta fields, shared
//     tool-call indices)
//
// If you find yourself special-casing a provider in this file, the right fix
// is almost always to make the server emit standard OpenAI shapes instead.
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

// wireMessage is the request-side OpenAI shape. Outbound only; the response
// is parsed via streamChunk below.
//
// Content has no omitempty: silent bash commands (e.g. heredoc writes) produce
// an empty tool-result string, and omitting the field causes Ollama's /v1 shim
// to 400 with "invalid message content type: <nil>". Always send an explicit
// string.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`         // tool name
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool role
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	// Index identifies which tool call a streaming delta belongs to. OpenAI
	// streams tool-call fragments across multiple chunks and each fragment
	// carries its index; slot lookup MUST key on this, not on slice position.
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

// streamOptions carries per-stream flags. The only one we use is
// include_usage: without it, OpenAI-compatible servers never emit the usage
// block in the SSE tail chunk and the per-turn counter sits at 0. Unknown
// flags are ignored by servers that don't support them.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk is one OpenAI SSE frame. finish_reason is intentionally not
// decoded: readSSE dispatches accumulated tool calls at stream end rather
// than reacting to finish_reason=="tool_calls", which keeps the path
// provider agnostic (Ollama's /v1 shim sometimes closes with "stop" even
// after streaming tool_calls).
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
	// Reasoning carries the incremental chain-of-thought fragment that
	// reasoning models (Qwen3, o1, ...) stream in `delta.reasoning` while
	// the server is still "thinking" and has not yet started producing
	// answer tokens. Forwarded as EventReasoning so the UI keeps animating
	// during the thinking phase. It never round-trips into the assistant
	// message — reasoning has no business being in conversation history.
	Reasoning string     `json:"reasoning,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

// Event is what the TUI consumes. One event per stream update.
type Event struct {
	Kind    EventKind
	Content string
	// ContextWindow carries the server-authoritative context size from the
	// X-Context-Window response header, populated only on EventDone (and
	// only when the server actually set the header). The TUI overwrites
	// the active profile's in-memory ContextSize with this value so the
	// next ctx.Pack uses what the server allows, not what config.yaml
	// guessed. Zero means "no live value in this response".
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
	// EventReasoning carries incremental reasoning text. Not part of the
	// assistant message that gets appended to history (reasoning stays
	// out of the transcript) — exists so the UI can tick its live token
	// estimate while the model is in the thinking phase.
	EventReasoning
)

type Client struct {
	BaseURL string
	Model   string
	Token   string // optional; empty = no Authorization header
	HTTP    *http.Client
	// noReasoningEffort is set to true once the server returns the
	// standard 400 saying it doesn't accept tools + reasoning_effort
	// together on /v1/chat/completions (OpenAI's gpt-5.5+ does this —
	// they push that combo onto /v1/responses). Sticky for the
	// lifetime of the Client so subsequent turns skip straight to the
	// supported shape. A `/models` switch builds a fresh Client and
	// the flag resets, which is correct: the new endpoint may be a
	// different provider with different rules.
	noReasoningEffort bool
}

func New(base, model, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(base, "/"),
		Model:   model,
		Token:   token,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

// ProbeResult is what Probe extracts from a one-shot hello-world request:
// the live context window the server allocates for this caller and the
// budget snapshot. The TUI uses these at activation time so the user sees
// their real context window in the confirmation line and the next ctx.Pack
// uses the server-authoritative number, not the config.yaml fallback.
type ProbeResult struct {
	ContextWindow int
	Budget        cloud.BudgetStatus
}

// Probe sends a minimal hello chat to /v1/chat/completions just to harvest
// response headers in one round trip: HTTP status validates the
// URL/model/key combo, X-Context-Window carries the live context size,
// X-Budget-Remaining carries the live budget fraction. The body is closed
// without reading any SSE — we only care about the headers, and on the
// cloud proxy that means generation may already be charged for one token,
// which is the cost of having a single round-trip "key works AND here is
// your real window". Returns the standard cloud errors (Unreachable,
// Unauthorized, BudgetExhausted) so callers can branch on them with
// errors.Is.
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

// Chat streams an assistant response. Events are delivered on the returned
// channel; the channel is closed when the stream ends. Reasoning runs at
// `high` effort by default; if the server explicitly rejects the
// tools + reasoning_effort combo on /v1/chat/completions (OpenAI's gpt-5.5+
// does), postChat drops reasoning_effort for the rest of this Client's
// lifetime so the model still works — without reasoning, but with tools.
// Staying on the chat-completions wire format is the product line; we do
// not branch to /v1/responses to preserve reasoning.
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

	budget := cloud.FromHeaders(resp.Header)
	ctxWindow := cloud.ContextWindowFromHeaders(resp.Header)
	final, tokens, err := readSSE(parent, resp.Body, budget, out)
	if err != nil {
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

// sendEvent puts e on out, bailing out if parent is cancelled first. Used
// everywhere run() and readSSE() publish so a slow / vanished consumer
// after Ctrl+C can never wedge the stream goroutine on a full-buffer send.
func sendEvent(parent context.Context, out chan<- Event, e Event) bool {
	select {
	case out <- e:
		return true
	case <-parent.Done():
		return false
	}
}

// sendChat POSTs the chat-completions request and returns the response on
// 200. On any failure path it returns the Event the caller should forward
// to the stream, already populated with the right Kind/Err/Budget. The
// response body is closed by the helper on every non-200 branch; the 200
// branch leaves it open for the caller's defer.
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

// postChat dispatches the chat request via doPost and on a standard 400
// saying the server doesn't accept tools + reasoning_effort together drops
// reasoning_effort for the lifetime of this Client and retries exactly
// once. Detection is loose substring matching on the response body —
// matching the param name plus "not supported" keeps the trigger precise
// without parsing JSON shapes that may differ between providers. Probe
// never sets ReasoningEffort, so its 400 path can never trip the flag.
func (c *Client) postChat(parent context.Context, body chatRequest) (*http.Response, cloud.BudgetStatus, error) {
	if c.noReasoningEffort {
		body.ReasoningEffort = ""
	}
	resp, budget, errBody, err := c.doPost(parent, body)
	if err != nil && body.ReasoningEffort != "" &&
		bytes.Contains(errBody, []byte("reasoning_effort")) &&
		bytes.Contains(errBody, []byte("not supported")) {
		c.noReasoningEffort = true
		body.ReasoningEffort = ""
		resp, budget, _, err = c.doPost(parent, body)
	}
	return resp, budget, err
}

// doPost performs one round-trip and maps response status into the typed
// cloud errors Probe and sendChat share. On 200 it returns the live
// response with its body still open for the caller's streaming reader; on
// any non-200 the body is drained and closed before returning. Budget is
// populated only on the 402 path, where the body carries a structured
// "budget exhausted" payload the UI surfaces. The errBody return is the
// raw error body bytes on a non-2xx other than 401/402, so postChat can
// inspect it for the reasoning_effort fallback signal without re-reading.
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
		return nil, cloud.BudgetStatus{}, nil, cloud.ErrUnauthorized
	case 402:
		// Pass is depleted. Body is ignored on purpose: the status code is
		// the whole signal, the UI banner is fixed text, and the budget
		// snapshot we return reflects the depletion directly.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, cloud.BudgetStatus{Set: true, Remaining: 0}, nil, cloud.ErrBudgetExhausted
	default:
		b, _ := io.ReadAll(resp.Body)
		return nil, cloud.BudgetStatus{}, b, fmt.Errorf("%d: %s", resp.StatusCode, errorMessageFromBody(b))
	}
}

// errorMessageFromBody extracts the user-facing string from a non-2xx body.
// codehamr.com's hamrpass proxy wraps every error in a structured envelope
// (`{"error":{"message":"...","provider_hint":"..."}}`) and we prefer
// `provider_hint` when present because providers stash their human-readable
// diagnostic there ("rate-limited, retry shortly"). Falls back to `message`,
// then to the raw first line so non-hamrpass backends (Ollama, bare OpenAI,
// etc.) keep surfacing whatever they emit.
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
// content/reasoning/tool-call events to out as they arrive. Returns the
// final assistant message (content + accumulated tool calls), the
// server-reported token count, and the scanner error if any. parent is
// threaded through so sends to out abort cleanly on cancellation rather
// than blocking on a buffer that nobody is draining any more.
func readSSE(parent context.Context, body io.Reader, budget cloud.BudgetStatus, out chan<- Event) (*chmctx.Message, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<16), 4<<20)

	var (
		fullContent strings.Builder
		slots       = map[int]*toolSlot{}
		order       []int
		tokens      int
	)

	for scanner.Scan() {
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

	// Emit accumulated tool calls exactly once, at stream end, independent
	// of finish_reason. Ollama's /v1 shim sometimes closes with "stop" even
	// when it just streamed tool_calls; dispatching here (instead of
	// in-loop on finish_reason=="tool_calls") makes the path provider
	// agnostic. Resolve every slot up front so the tool call payload is
	// parsed once and shared between the EventToolCall stream and the
	// final assistant message's ToolCalls.
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
// streamed tool-call fragments into their index-keyed slots. Reasoning
// stays out of fullContent — it must not round-trip back into the
// assistant message — but we forward it so the UI can reflect activity
// while the model is thinking. Tool-call fragments key on the provider's
// `index`, not on slice position, because a given call's fragments arrive
// across several chunks whose slice position need not match the index.
// Returns false when parent cancelled mid-send so the caller can bail.
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
			slot = &toolSlot{id: tc.ID}
			slots[tc.Index] = slot
			*order = append(*order, tc.Index)
		}
		if tc.Function.Name != "" {
			slot.name = tc.Function.Name
		}
		slot.args.WriteString(tc.Function.Arguments)
	}
	return true
}

// toolSlot accumulates one streamed tool call. OpenAI delivers the
// `arguments` field as a string of JSON that arrives in fragments across
// many SSE chunks — each fragment invalid on its own. We append them raw
// and parse exactly once, when resolve() is called.
type toolSlot struct {
	id, name string
	args     strings.Builder
}

func (t *toolSlot) resolve() chmctx.ToolCall {
	parsed := map[string]any{}
	if t.args.Len() > 0 {
		if err := json.Unmarshal([]byte(t.args.String()), &parsed); err != nil {
			// Malformed streamed args surface as a sentinel key rather
			// than a silently empty map so the tool-result log at least
			// names what broke. Real args never start with _parse_error,
			// so collisions aren't a concern.
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
