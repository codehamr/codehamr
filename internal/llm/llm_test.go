package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codehamr/codehamr/internal/cloud"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func collect(ch <-chan Event) []Event {
	var evs []Event
	for e := range ch {
		evs = append(evs, e)
	}
	return evs
}

// sseOK writes an OpenAI-style streamed response with the given chunks and a
// [DONE] terminator. The budget header travels on the 200 like in production.
func sseOK(w http.ResponseWriter, chunks []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Budget-Remaining", "0.73")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestChatStreamsContent: two content deltas merge into one final string.
func TestChatStreamsContent(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}],"usage":{"completion_tokens":7}}`,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "qwen3.5:27b", "sk-xyz")
	events := collect(c.Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	if gotAuth != "Bearer sk-xyz" {
		t.Fatalf("auth header missing: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"qwen3.5:27b"`) {
		t.Fatalf("model missing from request: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"reasoning_effort":"high"`) {
		t.Fatalf("reasoning_effort must default to 'high' (server-driven fallback only): %s", gotBody)
	}

	var content strings.Builder
	var sawDone bool
	for _, e := range events {
		switch e.Kind {
		case EventContent:
			content.WriteString(e.Content)
		case EventDone:
			sawDone = true
			if e.Final == nil || e.Final.Content != "Hello" {
				t.Errorf("final content wrong: %+v", e.Final)
			}
			if e.Tokens != 7 {
				t.Errorf("tokens = %d, want 7", e.Tokens)
			}
			if !e.Budget.Set || e.Budget.Remaining != 0.73 {
				t.Errorf("budget not propagated: %+v", e.Budget)
			}
		}
	}
	if content.String() != "Hello" {
		t.Fatalf("content = %q", content.String())
	}
	if !sawDone {
		t.Fatal("no done event")
	}
}

// TestChatToolCall: OpenAI tool_calls in an assistant delta emit an
// EventToolCall AND are carried in EventDone.Final.ToolCalls so the next
// turn can replay the assistant message into the conversation history.
func TestChatToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":5}}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var got *chmctx.ToolCall
	var final *chmctx.Message
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventToolCall:
			got = e.ToolCall
		case EventDone:
			final = e.Final
		}
	}
	if got == nil || got.Name != "bash" {
		t.Fatalf("tool call missing: %+v", got)
	}
	if cmd, _ := got.Arguments["cmd"].(string); cmd != "ls" {
		t.Fatalf("tool args wrong: %+v", got.Arguments)
	}
	if final == nil || len(final.ToolCalls) != 1 || final.ToolCalls[0].Name != "bash" {
		t.Fatalf("Final.ToolCalls should carry the bash call: %+v", final)
	}
}

// TestChatToolCallFragmentedArgs: OpenAI streams the `arguments` field as
// a JSON string that arrives in fragments, each invalid on its own. The
// client must accumulate raw and parse exactly once at finish_reason.
func TestChatToolCallFragmentedArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\":\"ls"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":6}}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var got *chmctx.ToolCall
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventToolCall {
			got = e.ToolCall
		}
	}
	if got == nil || got.Name != "bash" {
		t.Fatalf("tool call missing: %+v", got)
	}
	if cmd, _ := got.Arguments["cmd"].(string); cmd != "ls" {
		t.Fatalf("fragmented args not reassembled — wanted cmd=ls, got %+v", got.Arguments)
	}
	if len(got.Arguments) != 1 {
		t.Fatalf("expected exactly one parsed arg, got %+v", got.Arguments)
	}
}

// TestChatToolCallMultipleByIndex: two tool calls interleaved across chunks.
// The client must route each fragment to the correct slot via `index`, not
// via slice position within a chunk.
func TestChatToolCallMultipleByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"python"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"cmd\":\"print()\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var calls []chmctx.ToolCall
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventToolCall {
			calls = append(calls, *e.ToolCall)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 tool-call events, got %d: %+v", len(calls), calls)
	}
	byName := map[string]map[string]any{}
	for _, c := range calls {
		byName[c.Name] = c.Arguments
	}
	if cmd, _ := byName["bash"]["cmd"].(string); cmd != "ls" {
		t.Fatalf("bash args wrong: %+v", byName["bash"])
	}
	if cmd, _ := byName["python"]["cmd"].(string); cmd != "print()" {
		t.Fatalf("python args wrong: %+v", byName["python"])
	}
}

// TestChatDispatchesToolCallsOnFinishStop: Ollama's /v1 shim sometimes
// emits finish_reason="stop" even when tool_calls were just streamed. The
// client must still emit EventToolCall — otherwise the call vanishes into
// the void and the agent stares at an empty turn with nothing to do.
func TestChatDispatchesToolCallsOnFinishStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":5}}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var got *chmctx.ToolCall
	var final *chmctx.Message
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventToolCall:
			got = e.ToolCall
		case EventDone:
			final = e.Final
		}
	}
	if got == nil || got.Name != "bash" {
		t.Fatalf("tool call must emit on finish_reason=stop: %+v", got)
	}
	if cmd, _ := got.Arguments["cmd"].(string); cmd != "ls" {
		t.Fatalf("args not carried through: %+v", got.Arguments)
	}
	if final == nil || len(final.ToolCalls) != 1 {
		t.Fatalf("final should still carry the tool call: %+v", final)
	}
}

// TestChatToolCallMalformedArgsPreservesMarker: when the streamed
// `arguments` string isn't valid JSON (provider bug), the client must not
// silently hand the tool an empty args map — it surfaces a sentinel key
// so the resulting tool-result log at least names what went wrong.
func TestChatToolCallMalformedArgsPreservesMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{not-json"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var got *chmctx.ToolCall
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventToolCall {
			got = e.ToolCall
		}
	}
	if got == nil {
		t.Fatal("tool call event missing")
	}
	if _, ok := got.Arguments["_parse_error"]; !ok {
		t.Fatalf("_parse_error sentinel missing: %+v", got.Arguments)
	}
}

// TestToWireAlwaysSendsContent: silent tool results (empty stdout, e.g. from a
// heredoc write) must still serialize "content":"" — Ollama's /v1 shim 400s
// if the field is absent or null.
func TestToWireAlwaysSendsContent(t *testing.T) {
	msgs := []chmctx.Message{
		{Role: chmctx.RoleAssistant, Content: "", ToolCalls: []chmctx.ToolCall{
			{ID: "c1", Name: "bash", Arguments: map[string]any{"cmd": "true"}},
		}},
		{Role: chmctx.RoleTool, Content: "", ToolCallID: "c1", ToolName: "bash"},
	}
	buf, err := json.Marshal(toWire(msgs))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(buf)
	if !strings.Contains(got, `"role":"assistant","content":""`) {
		t.Errorf("assistant with tool_calls must keep content field: %s", got)
	}
	if !strings.Contains(got, `"role":"tool","content":""`) {
		t.Errorf("tool message with empty output must keep content field: %s", got)
	}
}

// TestChatSendsStreamIncludeUsage: OpenAI-compatible servers emit the usage
// block (completion_tokens) only when `stream_options.include_usage:true` is
// present in the request. Without it the per-turn token counter sits at 0.
// Every Chat call must ship the flag.
func TestChatSendsStreamIncludeUsage(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		sseOK(w, []string{`{"choices":[{"delta":{"content":"ok"}}]}`})
	}))
	defer srv.Close()
	collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	if !strings.Contains(gotBody, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("stream_options.include_usage missing from body: %s", gotBody)
	}
}

// TestChatReadsUsageTokens: the client reports tokens from the server's
// `usage.completion_tokens` field. Content length is irrelevant — we trust
// what the backend reports.
func TestChatReadsUsageTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"` + strings.Repeat("x", 100) + `"}}],"usage":{"completion_tokens":7}}`,
		})
	}))
	defer srv.Close()
	var tokens int
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventDone {
			tokens = e.Tokens
		}
	}
	if tokens != 7 {
		t.Fatalf("expected tokens=7 from usage block, got %d", tokens)
	}
}

// TestChat401: maps to typed ErrUnauthorized.
func TestChat401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || !errors.Is(evs[0].Err, cloud.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %+v", evs)
	}
}

// TestChat402: pass exhaustion surfaces as typed error and the budget
// snapshot reports zero remaining so the UI can paint the depleted state
// without waiting for the next response.
func TestChat402(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "k").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || !errors.Is(evs[0].Err, cloud.ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted, got %+v", evs)
	}
	if !evs[0].Budget.Set || evs[0].Budget.Remaining != 0 {
		t.Fatalf("budget snapshot should report zero remaining: %+v", evs[0].Budget)
	}
}

// TestChatUnreachable: transport failure surfaces as ErrUnreachable.
func TestChatUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1", "m", "")
	evs := collect(c.Chat(context.Background(), nil, nil))
	if len(evs) != 1 {
		t.Fatalf("want single event, got %d", len(evs))
	}
	var un cloud.ErrUnreachable
	if !errors.As(evs[0].Err, &un) {
		t.Fatalf("want ErrUnreachable, got %v", evs[0].Err)
	}
}

// TestChatOtherHTTPError: non-2xx (not 401/402) surfaces as a generic error
// with first line of body included in the message.
func TestChatOtherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "engine exploded")
		fmt.Fprintln(w, "see logs")
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || evs[0].Kind != EventError {
		t.Fatalf("want single error event, got %+v", evs)
	}
	if !strings.Contains(evs[0].Err.Error(), "500") ||
		!strings.Contains(evs[0].Err.Error(), "engine exploded") {
		t.Fatalf("error should include status and body excerpt: %v", evs[0].Err)
	}
	if strings.Contains(evs[0].Err.Error(), "see logs") {
		t.Fatalf("error should include only first body line: %v", evs[0].Err)
	}
}

// TestChatStructuredErrorPrefersProviderHint: codehamr.com's hamrpass proxy
// wraps upstream errors in `{"error":{"message":"...","provider_hint":"..."}}`
// and stashes the provider's user-facing diagnostic in `provider_hint`. The
// client must surface that hint over the generic message so users see the
// useful "retry shortly" text, not "upstream rate limited".
func TestChatStructuredErrorPrefersProviderHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"upstream rate limited","type":"rate_limited","upstream_status":429,"provider_hint":"deepseek is temporarily rate-limited, retry shortly"}}`)
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || evs[0].Kind != EventError {
		t.Fatalf("want single error event, got %+v", evs)
	}
	if !strings.Contains(evs[0].Err.Error(), "429") {
		t.Fatalf("error should include upstream status: %v", evs[0].Err)
	}
	if !strings.Contains(evs[0].Err.Error(), "retry shortly") {
		t.Fatalf("error should surface provider_hint: %v", evs[0].Err)
	}
	if strings.Contains(evs[0].Err.Error(), "upstream rate limited") {
		t.Fatalf("provider_hint should win over message: %v", evs[0].Err)
	}
}

// TestChatStructuredErrorFallsBackToMessage: when the envelope carries only
// `error.message` (no provider_hint), surface that — not the raw JSON blob
// the user would otherwise see.
func TestChatStructuredErrorFallsBackToMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"upstream unavailable","type":"upstream_unavailable","upstream_status":503}}`)
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || evs[0].Kind != EventError {
		t.Fatalf("want single error event, got %+v", evs)
	}
	if !strings.Contains(evs[0].Err.Error(), "503") ||
		!strings.Contains(evs[0].Err.Error(), "upstream unavailable") {
		t.Fatalf("error should include status and message: %v", evs[0].Err)
	}
	if strings.Contains(evs[0].Err.Error(), `{"error"`) {
		t.Fatalf("raw envelope JSON must not leak through to the user: %v", evs[0].Err)
	}
}

// TestReasoningChunksAreEmitted: reasoning models (Qwen, o1, ...) stream
// chain-of-thought text in `delta.reasoning` while the server is still
// "thinking". Those chunks must be surfaced as EventReasoning rather than
// dropped by the decoder, otherwise the UI freezes for the entire
// reasoning phase. Reasoning text must NOT be folded into the assistant
// content (it has no business being in history).
func TestReasoningChunksAreEmitted(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"reasoning":"Hmm"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"reasoning":" OK"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":3}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseOK(w, chunks)
	}))
	defer srv.Close()

	evs := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	var reasoning, content string
	var done Event
	for _, e := range evs {
		switch e.Kind {
		case EventReasoning:
			reasoning += e.Content
		case EventContent:
			content += e.Content
		case EventDone:
			done = e
		}
	}
	if reasoning != "Hmm OK" {
		t.Fatalf("want reasoning %q, got %q", "Hmm OK", reasoning)
	}
	if content != "hi" {
		t.Fatalf("want content %q, got %q", "hi", content)
	}
	if done.Final == nil || done.Final.Content != "hi" {
		t.Fatalf("reasoning must not leak into final message content: %+v", done.Final)
	}
	if done.Tokens != 3 {
		t.Fatalf("want 3 tokens, got %d", done.Tokens)
	}
}

// TestChatFallsBackWhenReasoningEffortRejected: OpenAI's gpt-5.5+ rejects
// tools + reasoning_effort on /v1/chat/completions with a precise 400.
// postChat must drop reasoning_effort for the rest of the Client's life
// and retry once so the user's first turn still goes through. The flag is
// sticky — subsequent turns must not resend reasoning_effort or we'd burn
// a 400 on every prompt and tool call.
func TestChatFallsBackWhenReasoningEffortRejected(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"reasoning_effort"`) {
			w.WriteHeader(400)
			fmt.Fprintln(w, `{`)
			fmt.Fprintln(w, `  "error": {`)
			fmt.Fprintln(w, `    "message": "Function tools with reasoning_effort are not supported for gpt-5.5 in /v1/chat/completions. Please use /v1/responses instead.",`)
			fmt.Fprintln(w, `    "param": "reasoning_effort"`)
			fmt.Fprintln(w, `  }`)
			fmt.Fprintln(w, `}`)
			return
		}
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"completion_tokens":1}}`,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "gpt-5.5", "")

	// First turn: 400 → fallback → success.
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			t.Fatalf("first turn must succeed via fallback, got error: %v", e.Err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("first turn should send initial + retry (2 requests), got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"reasoning_effort"`) {
		t.Fatalf("first attempt should send reasoning_effort: %s", bodies[0])
	}
	if strings.Contains(bodies[1], `"reasoning_effort"`) {
		t.Fatalf("retry must drop reasoning_effort: %s", bodies[1])
	}

	// Second turn on the same Client: flag is sticky, no 400, no retry.
	bodies = nil
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			t.Fatalf("second turn must not error: %v", e.Err)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("second turn should make exactly 1 request, got %d", len(bodies))
	}
	if strings.Contains(bodies[0], `"reasoning_effort"`) {
		t.Fatalf("second turn must not resend reasoning_effort: %s", bodies[0])
	}
}
