package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/codehamr/codehamr/internal/config"
)

// newStub returns a Server whose stdio is wired to a fake MCP responder. The
// test supplies a function that replies to each incoming request line.
func newStub(t *testing.T, respond func(req rpcRequest) any) *Server {
	t.Helper()
	stdinR, stdinW := io.Pipe()   // server reads from stdinR
	stdoutR, stdoutW := io.Pipe() // server writes to stdoutW

	s := &Server{
		Name:  "fake",
		stdin: stdinW,
		scan:  bufio.NewScanner(stdoutR),
	}

	go func() {
		sc := bufio.NewScanner(stdinR)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			var req rpcRequest
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			out := respond(req)
			if out == nil {
				continue
			}
			b, _ := json.Marshal(out)
			_, _ = stdoutW.Write(append(b, '\n'))
		}
		_ = stdoutW.Close()
	}()
	t.Cleanup(func() { _ = stdinW.Close() })
	return s
}

func TestHandshakeAndListTools(t *testing.T) {
	s := newStub(t, func(req rpcRequest) any {
		switch req.Method {
		case "initialize":
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"protocolVersion":"2024-11-05"}`)}
		case "notifications/initialized":
			return nil
		case "tools/list":
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(
				`{"tools":[{"name":"echo","description":"echo back","inputSchema":{"type":"object"}}]}`)}
		case "tools/call":
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(
				`{"isError":false,"content":[{"type":"text","text":"pong"}]}`)}
		}
		return nil
	})

	if err := s.handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	tools, err := s.listTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools wrong: %+v", tools)
	}
	s.tools = tools

	out, err := s.Call(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("call output wrong: %q", out)
	}
}

func TestRPCError(t *testing.T) {
	s := newStub(t, func(req rpcRequest) any {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "method not found"}}
	})
	if _, err := s.call("whatever", nil); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManagerToolRoutingAndDispatch(t *testing.T) {
	// minimal Manager populated by hand (avoids the Spawn process requirement).
	m := NewManager()
	s := newStub(t, func(req rpcRequest) any {
		if req.Method == "tools/call" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)}
		}
		return nil
	})
	s.tools = []ToolDef{{Name: "lookup", Description: "d", InputSchema: map[string]any{"type": "object"}}}
	m.servers["s1"] = s
	m.toolIndex["lookup"] = "s1"

	srv, ok := m.Has("lookup")
	if !ok || srv != "s1" {
		t.Fatalf("has failed: %s %v", srv, ok)
	}
	tools := m.Tools()
	if len(tools) != 1 || tools[0].Function.Name != "lookup" {
		t.Fatalf("tools: %+v", tools)
	}
	out, err := m.Call(context.Background(), "s1", "lookup", map[string]any{"q": "hi"})
	if err != nil || out != "ok" {
		t.Fatalf("call: %q %v", out, err)
	}
}

// TestSpawnAllSkipsDisabled: SpawnAll iterates the config snapshot and
// quietly skips servers with Enabled=false. A misbehaving disabled entry
// must not produce a spawn error.
func TestSpawnAllSkipsDisabled(t *testing.T) {
	m := NewManager()
	errs := m.SpawnAll(map[string]config.MCPServer{
		"never-runs": {Command: "/path/that/does/not/exist", Enabled: false},
	})
	if len(errs) != 0 {
		t.Fatalf("disabled server must not be spawned, got errs: %v", errs)
	}
	if len(m.servers) != 0 {
		t.Fatalf("manager should remain empty, got %d servers", len(m.servers))
	}
}

// TestSpawnAllReportsErrorPerEnabled: a real Spawn against a non-existent
// command surfaces an error tagged with the server name. One bad server
// must not abort iteration over the others.
func TestSpawnAllReportsErrorPerEnabled(t *testing.T) {
	m := NewManager()
	errs := m.SpawnAll(map[string]config.MCPServer{
		"broken": {Command: "/definitely/missing/binary-x9z2", Enabled: true},
	})
	if len(errs) != 1 {
		t.Fatalf("want 1 error from broken server, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "broken") {
		t.Fatalf("error should name the offending server: %v", errs[0])
	}
}

// TestManagerStopUnknownIsNoOp: Stop on a name not in the registry must
// not panic and must not touch the toolIndex.
func TestManagerStopUnknownIsNoOp(t *testing.T) {
	m := NewManager()
	m.toolIndex["x"] = "ghost" // sentinel to make sure Stop does nothing
	m.Stop("ghost")
	if _, ok := m.toolIndex["x"]; !ok {
		t.Fatal("Stop on unknown server must leave toolIndex untouched")
	}
}

// TestManagerStopAllClearsRegistry: StopAll wipes both maps and calls Stop
// on each server. With a stub server (no real process), Stop is still safe
// because of the cmd==nil guard.
func TestManagerStopAllClearsRegistry(t *testing.T) {
	m := NewManager()
	stub := &Server{Name: "s", tools: []ToolDef{{Name: "t"}}}
	m.servers["s"] = stub
	m.toolIndex["t"] = "s"
	m.StopAll()
	if len(m.servers) != 0 || len(m.toolIndex) != 0 {
		t.Fatalf("StopAll left state behind: servers=%d index=%d",
			len(m.servers), len(m.toolIndex))
	}
}

// TestManagerStopRemovesToolIndex: after Stop, the named server's tools
// no longer appear in the dispatch index. Otherwise a Has() lookup would
// claim the tool exists, then Call() would error with "server not running"
// — confusing the agent's failure handling.
func TestManagerStopRemovesToolIndex(t *testing.T) {
	m := NewManager()
	stub := &Server{Name: "s", tools: []ToolDef{{Name: "lookup"}}}
	m.servers["s"] = stub
	m.toolIndex["lookup"] = "s"

	m.Stop("s")

	if _, ok := m.Has("lookup"); ok {
		t.Fatal("tool index entry must be removed when server stops")
	}
}

// TestManagerCallUnknownServer: Call against a server name that isn't in
// the registry returns an error containing the server name (so an agent
// log can name what it tried to reach).
func TestManagerCallUnknownServer(t *testing.T) {
	m := NewManager()
	_, err := m.Call(context.Background(), "ghost", "anything", nil)
	if err == nil {
		t.Fatal("Call on missing server must error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error should name the missing server: %v", err)
	}
}

// TestServerStopOnNilCmdIsNoOp: Stop on a server whose cmd was never
// populated (e.g. constructed via direct-init in tests) must early-return
// instead of crashing on a nil dereference. Also exercises the "stdin nil"
// branch.
func TestServerStopOnNilCmdIsNoOp(t *testing.T) {
	s := &Server{Name: "bare"} // no cmd, no stdin
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop on bare server panicked: %v", r)
		}
	}()
	s.Stop()
}

// TestCallParentCancelReturnsImmediately: when the parent context is
// cancelled before/while a tools/call is in flight, Call returns the
// context error promptly rather than blocking on the underlying RPC. The
// inner goroutine continues until the server replies (or never does) but
// the caller is unblocked.
func TestCallParentCancelReturnsImmediately(t *testing.T) {
	// Stub that never replies — tools/call hangs forever inside the inner
	// goroutine. Without the parent-cancel select, the outer Call would
	// block too.
	s := newStub(t, func(req rpcRequest) any {
		if req.Method == "tools/call" {
			return nil // server never answers
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front
	start := time.Now()
	_, err := s.Call(ctx, "anything", nil)
	if err == nil {
		t.Fatal("Call with cancelled parent must return an error")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Call with cancelled parent took too long: %s", time.Since(start))
	}
}

// TestCallParseErrorSurfaces: a malformed tools/call response (not a valid
// JSON content array) must surface as an error rather than silently
// returning an empty string the agent can't act on.
func TestCallParseErrorSurfaces(t *testing.T) {
	s := newStub(t, func(req rpcRequest) any {
		if req.Method == "tools/call" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"content":"not-an-array"}`)}
		}
		return nil
	})
	if _, err := s.Call(context.Background(), "x", nil); err == nil {
		t.Fatal("malformed tools/call response should surface as error")
	}
}

// TestCallIsErrorCarriesContent: an isError:true response carries the
// content text alongside the sentinel error so the dispatcher can hand
// the agent a useful diagnostic instead of "(tool error: tool reported
// error)".
func TestCallIsErrorCarriesContent(t *testing.T) {
	s := newStub(t, func(req rpcRequest) any {
		if req.Method == "tools/call" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"rate limited · retry in 30s"}]}`)}
		}
		return nil
	})
	out, err := s.Call(context.Background(), "x", nil)
	if err == nil {
		t.Fatal("isError:true must produce an error")
	}
	if !strings.Contains(out, "rate limited") {
		t.Fatalf("content must travel back with the error: out=%q err=%v", out, err)
	}
}

// TestManagerToolsSortedDeterministic: Tools() walks servers in sorted
// order so chat payloads (and prompt-cache keys) are stable across calls.
// Map iteration is intentionally randomised by Go.
func TestManagerToolsSortedDeterministic(t *testing.T) {
	m := NewManager()
	for _, name := range []string{"zebra", "alpha", "mango"} {
		m.servers[name] = &Server{Name: name, tools: []ToolDef{
			{Name: "tool-" + name, InputSchema: map[string]any{"type": "object"}},
		}}
	}
	first := m.Tools()
	for i := 0; i < 20; i++ {
		ts := m.Tools()
		if len(ts) != len(first) {
			t.Fatalf("Tools length jittered: %d vs %d", len(ts), len(first))
		}
		for i, tt := range ts {
			if tt.Function.Name != first[i].Function.Name {
				t.Fatalf("Tools order jittered at %d: %q vs %q",
					i, tt.Function.Name, first[i].Function.Name)
			}
		}
	}
	// And the order is sorted-by-server-name, not insertion order.
	want := []string{"tool-alpha", "tool-mango", "tool-zebra"}
	for i, w := range want {
		if first[i].Function.Name != w {
			t.Fatalf("position %d: got %q, want %q", i, first[i].Function.Name, w)
		}
	}
}

// TestManagerConcurrentAccess exercises the mutex that guards servers /
// toolIndex. The real race is SpawnAll (goroutine) vs Tools/Has/Call
// (TUI goroutine); we simulate the same pattern by hammering the manager
// from multiple goroutines while direct-populating and direct-removing
// entries. Run with -race to catch a regression where the lock goes away.
func TestManagerConcurrentAccess(t *testing.T) {
	m := NewManager()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			name := "srv"
			s := &Server{Name: name, tools: []ToolDef{
				{Name: "t" + string(rune('a'+(i%26))), InputSchema: map[string]any{"type": "object"}},
			}}
			m.mu.Lock()
			m.servers[name] = s
			for _, td := range s.tools {
				m.toolIndex[td.Name] = name
			}
			m.mu.Unlock()
			m.mu.Lock()
			for _, td := range s.tools {
				delete(m.toolIndex, td.Name)
			}
			delete(m.servers, name)
			m.mu.Unlock()
		}
	}()
	for i := 0; i < 500; i++ {
		_ = m.Tools()
		_, _ = m.Has("ta")
	}
	<-done
}
