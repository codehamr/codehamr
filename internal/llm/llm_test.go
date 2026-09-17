package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// sseOK writes an OpenAI-style streamed response plus a [DONE] terminator. The
// budget header travels on the 200 like in production.
func sseOK(w http.ResponseWriter, chunks []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Budget-Remaining", "0.73")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestChatStreamsContent: content deltas merge into one final string.
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

	c := New(srv.URL, "test-model", "sk-xyz")
	events := collect(c.Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	if gotAuth != "Bearer sk-xyz" {
		t.Fatalf("auth header missing: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"test-model"`) {
		t.Fatalf("model missing from request: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"reasoning_effort":"medium"`) {
		t.Fatalf("reasoning_effort must default to 'medium' (decode is the serialised critical path): %s", gotBody)
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

// TestChatToolCall: tool_calls in a delta emit EventToolCall and ride along in
// EventDone.Final.ToolCalls so the next turn can replay the assistant message.
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

// TestChatToolCallFragmentedArgs: `arguments` arrives as JSON fragments, each
// invalid alone. The client must accumulate raw and parse once at finish_reason.
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
		t.Fatalf("fragmented args not reassembled - wanted cmd=ls, got %+v", got.Arguments)
	}
	if len(got.Arguments) != 1 {
		t.Fatalf("expected exactly one parsed arg, got %+v", got.Arguments)
	}
}

// TestChatToolArgsStreamLive: each tool-call arguments fragment is forwarded as
// an EventToolArgs as it arrives, so the UI can tick its live token estimate
// while a file streams into write_file, not just once at stream end. Fragments
// concatenate to the full arguments and all precede the resolved EventToolCall.
func TestChatToolArgsStreamLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write_file"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".txt\",\"content\":\"hi"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var args strings.Builder
	sawCall := false
	argsAllBeforeCall := true
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventToolArgs:
			args.WriteString(e.Content)
			if sawCall {
				argsAllBeforeCall = false
			}
		case EventToolCall:
			sawCall = true
		}
	}
	if got := args.String(); got != `{"path":"a.txt","content":"hi"}` {
		t.Fatalf("EventToolArgs fragments should concatenate to the full args, got %q", got)
	}
	if !sawCall {
		t.Fatalf("expected a resolved EventToolCall after the fragments")
	}
	if !argsAllBeforeCall {
		t.Fatalf("every EventToolArgs must precede the resolved EventToolCall")
	}
}

// TestChatToolCallMultipleByIndex: two tool calls interleaved across chunks.
// Each fragment must route to its slot by `index`, not by slice position.
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

// TestChatDispatchesToolCallsOnFinishStop: Ollama's /v1 shim sometimes emits
// finish_reason="stop" even after streaming tool_calls. The client must still
// emit EventToolCall, or the call vanishes and the agent faces an empty turn.
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

// TestChatToolCallLateIDPreserved: spec ships the tool_call `id` in the first
// fragment, but a sloppy provider may delay it. The client must update slot.id
// on any non-empty value (same forgiveness as `name`), else the id stays "" and
// the next /v1 request 400s on the unpaired tool message.
func TestChatToolCallLateIDPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
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
	if got.ID != "call_late" {
		t.Fatalf("late-arriving id lost: got %q, want %q", got.ID, "call_late")
	}
}

// TestChatToolCallMalformedArgsPreservesMarker: on invalid `arguments` JSON
// (provider bug), the client surfaces a sentinel key rather than an empty args
// map, so the tool-result log names what went wrong.
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

// TestToWireAlwaysSendsContent: silent tool results (empty stdout) must still
// serialize "content":"". Ollama's /v1 shim 400s if the field is absent or null.
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

// TestChatSendsStreamIncludeUsage: servers emit the usage block only when
// `stream_options.include_usage:true` is in the request; without it the per-turn
// token counter sits at 0. Every Chat call must ship the flag.
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

// TestChatMidStreamErrorFrameSurfacesAsError: OpenAI-compatible backends (and
// OpenRouter-style proxies like the hosted endpoint) report a post-200
// provider failure as a final `data: {"error":{...}}` frame followed by
// connection close with no [DONE]. That frame must surface as EventError; left
// undecoded it parses to zero choices, the close reads as clean EOF, and a
// mid-sentence-truncated turn finalizes as a confident EventDone success.
func TestChatMidStreamErrorFrameSurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"error\":{\"code\":502,\"message\":\"Provider returned error\"}}\n\n")
		// connection closes without [DONE]
	}))
	defer srv.Close()

	events := collect(New(srv.URL, "m", "").Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	last := events[len(events)-1]
	if last.Kind != EventError {
		t.Fatalf("stream must end in EventError, got kind %v (events: %+v)", last.Kind, events)
	}
	if !strings.Contains(last.Err.Error(), "Provider returned error") {
		t.Fatalf("error must carry the server's message, got %v", last.Err)
	}
	for _, e := range events {
		if e.Kind == EventDone {
			t.Fatal("a stream that died mid-generation must not emit EventDone")
		}
	}
}

// TestChatReadsUsageTokens: tokens come from `usage.completion_tokens` (and
// prompt_tokens rides along for the debug-log calibration), not content
// length; we trust what the backend reports.
func TestChatReadsUsageTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"` + strings.Repeat("x", 100) + `"}}],"usage":{"completion_tokens":7,"prompt_tokens":42}}`,
		})
	}))
	defer srv.Close()
	var tokens, promptTokens int
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventDone {
			tokens = e.Tokens
			promptTokens = e.PromptTokens
		}
	}
	if tokens != 7 {
		t.Fatalf("expected tokens=7 from usage block, got %d", tokens)
	}
	if promptTokens != 42 {
		t.Fatalf("expected prompt_tokens=42 from usage block, got %d", promptTokens)
	}
}

// TestSendEventUnblocksOnCancel pins sendEvent's anti-wedge invariant: once the
// parent context is cancelled, a send to an undrained channel must abort via the
// <-parent.Done() arm instead of blocking the stream goroutine forever. This is
// the only path exercising that arm. A regression to a plain `out <- e` leaks the
// Chat goroutine on Ctrl+C against a full buffer; the goroutine never returns and
// the deadline below fires.
func TestSendEventUnblocksOnCancel(t *testing.T) {
	out := make(chan Event) // unbuffered, no reader → the send blocks until cancel
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() {
		done <- sendEvent(ctx, out, Event{Kind: EventContent, Content: "nobody is reading me"})
	}()

	// The send is wedged (no reader on out); cancelling must release it.
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("sendEvent returned true for a send nobody drained; it must report false after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendEvent wedged on an undrained channel after cancel - the anti-wedge <-parent.Done() arm is missing")
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

// TestChat401DrainsBodyForConnReuse: a 401 carrying a body must have that body
// drained before close, or Go's transport discards the TCP connection instead
// of returning it to the keep-alive pool. With the same client issuing two
// sequential 401s, a drained body reuses one connection (one RemoteAddr); an
// undrained one forces a fresh connection on the second request (two). The 402
// and default error branches already drain; this pins the 401 branch to match.
func TestChat401DrainsBodyForConnReuse(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		// Non-empty body: an empty 401 is reusable regardless and would hide the
		// regression. A real backend's 401 carries an error JSON like this.
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	for i := 0; i < 2; i++ {
		evs := collect(c.Chat(context.Background(), nil, nil))
		if len(evs) != 1 || !errors.Is(evs[0].Err, cloud.ErrUnauthorized) {
			t.Fatalf("request %d: want ErrUnauthorized, got %+v", i, evs)
		}
	}
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("401 body not drained: server saw %d connections across 2 sequential requests, want 1 (keep-alive reuse defeated)", n)
	}
}

// TestChat402: budget exhaustion surfaces as a typed error with the snapshot
// reporting zero remaining, so the UI paints the depleted state immediately.
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

// TestChatUnreachable: transport failure surfaces as ErrUnreachable. Retries
// off: this test pins the terminal error surface, not the retry path.
func TestChatUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1", "m", "")
	c.RetryBackoff = nil
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
// carrying only the first body line. Retries off: this test pins the terminal
// error surface, not the retry path.
func TestChatOtherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "engine exploded")
		fmt.Fprintln(w, "see logs")
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	c.RetryBackoff = nil
	evs := collect(c.Chat(context.Background(), nil, nil))
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

// TestChatStructuredErrorPrefersProviderHint: the hamrpass proxy wraps upstream
// errors as `{"error":{"message":...,"provider_hint":...}}`. The client must
// surface provider_hint over message, so users see the useful "retry shortly"
// text, not "upstream rate limited".
func TestChatStructuredErrorPrefersProviderHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"upstream rate limited","type":"rate_limited","upstream_status":429,"provider_hint":"the upstream model is temporarily rate-limited, retry shortly"}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	c.RetryBackoff = nil // pin the terminal error surface, not the retry path
	evs := collect(c.Chat(context.Background(), nil, nil))
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

// TestChatStructuredErrorFallsBackToMessage: with only `error.message` (no
// provider_hint), surface that, not the raw JSON envelope.
func TestChatStructuredErrorFallsBackToMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"upstream unavailable","type":"upstream_unavailable","upstream_status":503}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	c.RetryBackoff = nil // pin the terminal error surface, not the retry path
	evs := collect(c.Chat(context.Background(), nil, nil))
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

// TestReasoningChunksAreEmitted: reasoning models stream chain-of-thought in
// `delta.reasoning`. The decoder must surface these as EventReasoning (else the
// UI freezes for the whole reasoning phase) and must NOT fold them into the
// assistant content: reasoning has no business in history.
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

// TestChatFallsBackWhenReasoningEffortRejected: newer OpenAI models reject tools +
// reasoning_effort on /v1/chat/completions with a 400. postChat must drop the
// field, retry once, and stay sticky for the Client's life, else every turn
// burns a 400.
func TestChatFallsBackWhenReasoningEffortRejected(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"reasoning_effort"`) {
			w.WriteHeader(400)
			fmt.Fprintln(w, `{`)
			fmt.Fprintln(w, `  "error": {`)
			fmt.Fprintln(w, `    "message": "Function tools with reasoning_effort are not supported for this model in /v1/chat/completions. Please use /v1/responses instead.",`)
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

	c := New(srv.URL, "cloud-model", "")

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

// TestChatFallsBackToNoneWhenServerDemandsIt: from GPT-5.4 on, OpenAI's
// default effort is a real level, so a retry that merely omits the field 400s
// identically ("set reasoning_effort to 'none'"). The fallback must send the
// value the message names, and keep sending it on later turns.
func TestChatFallsBackToNoneWhenServerDemandsIt(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if !strings.Contains(string(b), `"reasoning_effort":"none"`) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","param":"reasoning_effort"}}`)
			return
		}
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"completion_tokens":1}}`,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "gpt-6-astra", "")
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			t.Fatalf("first turn must succeed via fallback, got error: %v", e.Err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("first turn should send initial + retry (2 requests), got %d", len(bodies))
	}
	if !strings.Contains(bodies[1], `"reasoning_effort":"none"`) {
		t.Fatalf("retry must send reasoning_effort none, not omit it: %s", bodies[1])
	}

	bodies = nil
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			t.Fatalf("second turn must not error: %v", e.Err)
		}
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"reasoning_effort":"none"`) {
		t.Fatalf("second turn should make 1 request with none pinned, got %v", bodies)
	}
}

// TestProbeChatNoReasoningEffortIsRaceFree pins the atomic guard on
// Client.reasoningFallback. The startup probe and the first chat can run on the
// same *Client concurrently (probe from Init, chat when the user submits early):
// both read the flag via postChat, and a 400 fallback writes it. A plain bool
// would be a data race; this must run clean under -race.
func TestProbeChatNoReasoningEffortIsRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// Force the 400 → write-the-flag branch on every chat still shipping
		// reasoning_effort, so concurrent Chat goroutines hit the write path.
		if strings.Contains(string(b), `"reasoning_effort"`) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"reasoning_effort not supported"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = c.Probe(context.Background())
		}()
		go func() {
			defer wg.Done()
			for range c.Chat(context.Background(), nil, nil) {
			}
		}()
	}
	wg.Wait()
}

// TestChatFallsBackWhenOllamaRejectsThinking: Ollama rejects reasoning_effort on
// non-thinking models with a 400 saying `<model> does not support thinking`:
// different shape from OpenAI's message, same remedy. postChat must drop the
// field, retry once, and stay sticky so we don't re-trip the 400 every turn.
func TestChatFallsBackWhenOllamaRejectsThinking(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"reasoning_effort"`) {
			w.WriteHeader(400)
			fmt.Fprintln(w, `{"error":"\"test-model:latest\" does not support thinking"}`)
			return
		}
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"completion_tokens":1}}`,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model:latest", "")

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

// TestChatFallsBackWhenEffortValueUnsupported: models that define their own
// effort scale 400 on a value outside it. Qwen3.8 (vLLM) ships xhigh/medium/low
// and has no `high`, the exact value Chat sends, so every turn died on a 400
// while the two older rejection shapes sailed past the matcher. Body is the
// server's verbatim response. Same remedy: drop the field, retry once, stay
// sticky — the server then applies its own default (xhigh here).
func TestChatFallsBackWhenEffortValueUnsupported(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"reasoning_effort"`) {
			w.WriteHeader(400)
			fmt.Fprintln(w, `{"error":{"message":"Unexpected reasoning effort high. Supported types are xhigh (default), medium, and low.","type":"BadRequestError","param":null,"code":400}}`)
			return
		}
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"completion_tokens":1}}`,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "Qwen3.8-27B", "")

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

// TestChatDoesNotFallBackOnUnrelatedThinking: a 400 that is NOT about reasoning
// but happens to contain both "not support" and the word "thinking" must not
// trip the reasoning_effort fallback. Otherwise the bare-"thinking" match
// burns a wasted retry and latches reasoning off for the Client's whole life on
// an error that had nothing to do with reasoning. The retry would resend the
// same (still-failing) request, so we'd see a second request and a sticky flag.
func TestChatDoesNotFallBackOnUnrelatedThinking(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		// Unrelated 400: the tool format is unsupported; "thinking" appears only
		// incidentally in the human-readable hint, not as a reasoning rejection.
		w.WriteHeader(400)
		fmt.Fprintln(w, `{"error":{"message":"the requested tool format is not supported","provider_hint":"thinking about it differently won't help"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "some-model", "")
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		_ = e // the turn errors (the 400 is real), that's expected
	}
	if len(bodies) != 1 {
		t.Fatalf("unrelated 400 must NOT trigger a fallback retry; got %d requests", len(bodies))
	}
	if c.reasoningFallback.Load() != nil {
		t.Fatal("reasoning must not latch off on a 400 unrelated to reasoning")
	}
}

// TestNewHasNoHTTPTimeout pins that the streaming Client must NOT set
// http.Client.Timeout: that field is end-to-end (it covers body reads) and would
// abort a legitimately slow SSE stream with "context deadline exceeded … while
// reading body" on slow local backends. Per-turn context cancellation governs
// request lifetime; this stops a refactor from reintroducing the wall-clock cap.
func TestNewHasNoHTTPTimeout(t *testing.T) {
	c := New("http://example.test", "model", "token")
	if c.HTTP.Timeout != 0 {
		t.Fatalf("http.Client.Timeout must be 0 so per-turn context governs SSE lifetime; got %v", c.HTTP.Timeout)
	}
}

// TestIdleTimeoutFromEnv pins the CODEHAMR_IDLE_TIMEOUT contract: a Go duration
// or bare-seconds string wins, anything else (unset, garbage, non-positive)
// falls back to the default. The default is deliberately generous because this
// is a dead-connection detector, not a loop guard.
func TestIdleTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		set  bool
		want time.Duration
	}{
		{set: false, want: streamIdleTimeout},
		{val: "", set: true, want: streamIdleTimeout},
		{val: "90m", set: true, want: 90 * time.Minute},
		{val: "1h30m", set: true, want: 90 * time.Minute},
		{val: "300", set: true, want: 300 * time.Second},
		{val: "garbage", set: true, want: streamIdleTimeout},
		{val: "0", set: true, want: streamIdleTimeout},
		{val: "-5m", set: true, want: streamIdleTimeout},
		// Bare seconds large enough to wrap the ×time.Second multiply to a
		// small POSITIVE duration must fall back, not silently kill every
		// live-but-slow stream mid-prefill.
		{val: "18446744074", set: true, want: streamIdleTimeout},
		{val: "9000000000", set: true, want: 9_000_000_000 * time.Second},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("CODEHAMR_IDLE_TIMEOUT", tc.val)
		} else {
			os.Unsetenv("CODEHAMR_IDLE_TIMEOUT")
		}
		if got := idleTimeoutFromEnv(); got != tc.want {
			t.Errorf("idleTimeoutFromEnv(%q, set=%v) = %v, want %v", tc.val, tc.set, got, tc.want)
		}
	}
}

// TestChatIdleTimeoutAbortsStalledStream reproduces the exact hang: the server
// returns 200 OK then sends nothing. Without the idle watchdog scanner.Scan()
// blocks forever; with it, the body is closed and the turn ends in an EventError
// naming the stall, a finite escape that doesn't need Ctrl+C.
func TestChatIdleTimeoutAbortsStalledStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush() // headers out so Client.Do returns; then go silent
		<-r.Context().Done()     // hang until the client gives up and closes the body
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.IdleTimeout = 60 * time.Millisecond

	start := time.Now()
	var gotErr error
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			gotErr = e.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected an EventError from the idle watchdog")
	}
	if !strings.Contains(gotErr.Error(), "stopped sending") {
		t.Fatalf("error should name the stall: %v", gotErr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("watchdog fired too late (%v) - should be ~IdleTimeout", elapsed)
	}
}

// TestChatIdleWatchdogResetByFrames pins that an alive-but-slow stream is NOT
// aborted: frames spaced under the idle window each reset the watchdog, so a
// stream whose total span exceeds one window still completes. Guards against a
// regression where onFrame() stops resetting the timer.
func TestChatIdleWatchdogResetByFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"delta":{"content":"A"}}]}`,
			`{"choices":[{"delta":{"content":"B"}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flush.Flush()
			time.Sleep(250 * time.Millisecond) // < IdleTimeout, so the watchdog resets
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.IdleTimeout = 400 * time.Millisecond // > each 250ms gap, < ~500ms total span

	var content strings.Builder
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventContent:
			content.WriteString(e.Content)
		case EventError:
			t.Fatalf("watchdog aborted a live stream: %v", e.Err)
		}
	}
	if content.String() != "AB" {
		t.Fatalf("content = %q, want AB (slow-but-alive stream must complete)", content.String())
	}
}

// TestToWireParseErrorArgsStayValidJSON: when resolve() stamps _parse_error for a
// truncated tool call and that assistant message round-trips into the next
// request, toWire must still emit VALID JSON for arguments. Otherwise every
// later turn re-sends corrupt JSON and the backend 400s forever (session
// poisoning). The protection is re-marshalling the parsed map, never raw bytes.
func TestToWireParseErrorArgsStayValidJSON(t *testing.T) {
	msgs := []chmctx.Message{{
		Role: chmctx.RoleAssistant,
		ToolCalls: []chmctx.ToolCall{{
			ID:        "c1",
			Name:      "write_file",
			Arguments: map[string]any{"_parse_error": "unexpected end of JSON input"},
		}},
	}}
	args := toWire(msgs)[0].ToolCalls[0].Function.Arguments
	if !json.Valid([]byte(args)) {
		t.Fatalf("arguments must stay valid JSON to avoid poisoning the session: %q", args)
	}
}

// TestChatRetriesTransient404: a proxy that hiccups (two 404s, then a clean
// stream) is retried transparently. The user sees one EventRetry per wait,
// then normal content — never an EventError. Pins the issue-#7 fix: LiteLLM
// fronting Agnes AI emits transient 404s and the turn used to die on the first.
func TestChatRetriesTransient404(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"Not Found"}`)
			return
		}
		sseOK(w, []string{`{"choices":[{"delta":{"content":"ok"}}]}`})
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	evs := collect(c.Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two failures + one success)", attempts)
	}
	var retries int
	var sawDone bool
	for _, e := range evs {
		switch e.Kind {
		case EventRetry:
			retries++
			if e.Content == "" || e.Err == nil {
				t.Errorf("retry event must carry a status hint and the trigger error: %+v", e)
			}
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("recovered turn must not surface an error: %v", e.Err)
		}
	}
	if retries != 2 {
		t.Fatalf("retry events = %d, want 2", retries)
	}
	if !sawDone {
		t.Fatal("no done event after successful retry")
	}
}

// TestChatRetryGivesUpAfterBackoffExhausted: a permanently failing backend gets
// len(RetryBackoff) retries, then the last error surfaces and the turn unwinds
// back to the user.
func TestChatRetryGivesUpAfterBackoffExhausted(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "down")
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	evs := collect(c.Chat(context.Background(), nil, nil))

	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4 (initial + 3 retries)", attempts)
	}
	last := evs[len(evs)-1]
	if last.Kind != EventError || !strings.Contains(last.Err.Error(), "503") {
		t.Fatalf("want final EventError carrying the 503, got %+v", last)
	}
}

// TestChatDoesNotRetryPermanentErrors: auth (401), depleted pass (402), and
// malformed request (400) fail identically on every resend; exactly one
// attempt, straight to EventError.
func TestChatDoesNotRetryPermanentErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := New(srv.URL, "m", "")
			c.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			evs := collect(c.Chat(context.Background(), nil, nil))
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1 (no retry on %d)", attempts, status)
			}
			if len(evs) != 1 || evs[0].Kind != EventError {
				t.Fatalf("want single error event, got %+v", evs)
			}
		})
	}
}

// TestChatRetryWaitCancelable: Ctrl+C during a backoff wait must unwind the
// turn immediately, not after the remaining sleep. The wait select must have a
// parent.Done() arm.
func TestChatRetryWaitCancelable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.RetryBackoff = []time.Duration{time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	ch := c.Chat(ctx, nil, nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	collect(ch) // channel must close promptly once cancel fires
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry wait ignored cancellation: channel closed after %s", elapsed)
	}
}

// TestRetryableClassification: network errors and transient statuses retry;
// typed sentinels (401/402), client errors, and unknown errors do not. 404 is
// deliberately in the retry set (LiteLLM proxies emit it transiently, #7).
func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unreachable", cloud.ErrUnreachable{Err: errors.New("refused")}, true},
		{"404", &httpStatusError{status: 404, msg: "not found"}, true},
		{"408", &httpStatusError{status: 408, msg: "timeout"}, true},
		{"429", &httpStatusError{status: 429, msg: "rate limited"}, true},
		{"500", &httpStatusError{status: 500, msg: "boom"}, true},
		{"503", &httpStatusError{status: 503, msg: "down"}, true},
		{"400", &httpStatusError{status: 400, msg: "bad request"}, false},
		{"403", &httpStatusError{status: 403, msg: "forbidden"}, false},
		{"unauthorized", cloud.ErrUnauthorized, false},
		{"depleted", cloud.ErrBudgetExhausted, false},
		{"misc", errors.New("something"), false},
	}
	for _, tc := range cases {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestProbeDoesNotRetry: the startup probe exists for fast feedback on a
// misconfigured URL/model/key; it must fail on the first response, never sit
// out retry backoffs.
func TestProbeDoesNotRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	if _, err := c.Probe(context.Background()); err == nil {
		t.Fatal("probe against a 404 backend must fail")
	}
	if attempts != 1 {
		t.Fatalf("probe attempts = %d, want 1", attempts)
	}
}

// TestChatParallelToolCallsSharedIndex: a backend that emits index 0 for every
// call in a parallel batch must still resolve N distinct calls. Concatenating
// their argument bodies into one slot yields a single _parse_error, which is
// how a batched round degrades into a stalled turn on exactly the local
// servers this project targets.
func TestChatParallelToolCallsSharedIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":9}}`,
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
		t.Fatalf("shared-index parallel batch resolved %d calls, want 2: %+v", len(calls), calls)
	}
	for i, want := range []string{"a.go", "b.go"} {
		if got, _ := calls[i].Arguments["path"].(string); got != want {
			t.Fatalf("call %d path = %q, want %q (args: %+v)", i, got, want, calls[i].Arguments)
		}
	}
}

// TestChatMidStreamDropIsReplayable / ...ServerErrorIsNot: the TUI replays a
// dropped socket but must never replay the server's own refusal, which would
// repeat forever.
func TestChatMidStreamDropIsReplayable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		srv2, _, _ := w.(http.Hijacker).Hijack()
		srv2.Close() // drop the socket mid-stream
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var errEvt *Event
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			errEvt = &e
		}
	}
	if errEvt == nil {
		t.Fatal("expected an EventError for a dropped stream")
	}
	if !errEvt.MidStream {
		t.Fatalf("a drop after a delivered frame must be replayable: %v", errEvt.Err)
	}
}

func TestChatServerStreamErrorIsNotReplayable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"content":"partial"}}]}`,
			`{"error":{"message":"context length exceeded"}}`,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var errEvt *Event
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			errEvt = &e
		}
	}
	if errEvt == nil {
		t.Fatal("expected an EventError for a server-reported stream error")
	}
	if errEvt.MidStream {
		t.Fatal("a server-reported error must never be replayed: it would repeat forever")
	}
}

// TestChatParallelToolCallsSharedIndexFragmented: the shared-index case with
// FRAGMENTED arguments. The continuation fragments carry the shared index and
// no id, so they must reach the call currently being streamed, not the earlier
// one parked under it. Getting this backwards silently welds one call's
// arguments onto another - valid JSON in, garbage out.
func TestChatParallelToolCallsSharedIndexFragmented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"b.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":9}}`,
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
		t.Fatalf("fragmented shared-index batch resolved %d calls, want 2: %+v", len(calls), calls)
	}
	for i, want := range []string{"a.go", "b.go"} {
		if got, _ := calls[i].Arguments["path"].(string); got != want {
			t.Fatalf("call %d (id %s) path = %q, want %q", i, calls[i].ID, got, want)
		}
	}
	if calls[0].ID != "c1" || calls[1].ID != "c2" {
		t.Fatalf("emission order lost: got ids %s, %s", calls[0].ID, calls[1].ID)
	}
}

// TestKeepaliveDoesNotCollapseThePrefillWindow: a proxy that emits one comment
// or blank line at 200 OK (LiteLLM/nginx SSE shims, OpenRouter's ": OPENROUTER
// PROCESSING") is liveness, not output. Treating it as output would (a) swap the
// long prefill window for the short inter-frame one while the model is still
// prefilling, and (b) mark the resulting DETERMINISTIC prefill stall as a
// replayable mid-stream drop - so the TUI would replay the same doomed prefill
// twice more before showing the user anything.
func TestKeepaliveDoesNotCollapseThePrefillWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		preamble string
		want     bool
	}{
		{"keepalive comment then silence", ": OPENROUTER PROCESSING\n\n", false},
		{"blank line then silence", "\n", false},
		{"real data frame then silence", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tc.preamble)
				w.(http.Flusher).Flush()
				// Go silent until the watchdog closes the body: an alive-but-mute
				// server, which is what a prefill looks like from out here.
				<-r.Context().Done()
			}))
			defer srv.Close()
			c := New(srv.URL, "m", "")
			c.IdleTimeout = 300 * time.Millisecond
			var errEvt *Event
			for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
				if e.Kind == EventError {
					errEvt = &e
				}
			}
			if errEvt == nil {
				t.Fatal("expected a stall error")
			}
			if errEvt.MidStream != tc.want {
				t.Fatalf("MidStream = %v, want %v (only a data frame means the model started producing): %v",
					errEvt.MidStream, tc.want, errEvt.Err)
			}
		})
	}
}

// TestChatCleanEOFWithoutCompletionSignalIsMidStreamDrop: a proxy/LB that
// gracefully closes the upstream mid-generation produces a clean EOF with no
// [DONE], no finish_reason, and no usage frame. That must surface as a
// MidStream EventError (so the TUI's bounded replay re-issues the request),
// never as EventDone: finalizing it hands the turn a mid-sentence-truncated
// assistant message as a clean finish - a transport-level false green no
// nudge can see.
func TestChatCleanEOFWithoutCompletionSignalIsMidStreamDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial ans\"}}]}\n\n")
		// connection closes cleanly: no [DONE], no finish_reason, no usage
	}))
	defer srv.Close()

	events := collect(New(srv.URL, "m", "").Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	last := events[len(events)-1]
	if last.Kind != EventError {
		t.Fatalf("signal-less EOF must end in EventError, got kind %v (events: %+v)", last.Kind, events)
	}
	if !last.MidStream {
		t.Fatalf("frames arrived before the cut, so the drop must be replayable (MidStream), got %v", last.Err)
	}
	for _, e := range events {
		if e.Kind == EventDone {
			t.Fatal("a cut stream must not emit EventDone")
		}
	}
}

// TestChatFinishReasonAloneCompletesStream: a backend that omits [DONE] but
// closes its last frame with a finish_reason (any value - Ollama's shim emits
// "stop" even after tool calls) finished on purpose; the turn must complete
// cleanly, not be treated as a drop.
func TestChatFinishReasonAloneCompletesStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		// no [DONE], no usage
	}))
	defer srv.Close()

	events := collect(New(srv.URL, "m", "").Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	last := events[len(events)-1]
	if last.Kind != EventDone {
		t.Fatalf("finish_reason is a completion signal; want EventDone, got kind %v (err: %v)", last.Kind, last.Err)
	}
	if last.Final == nil || last.Final.Content != "done answer" {
		t.Fatalf("final message must carry the streamed content, got %+v", last.Final)
	}
}
