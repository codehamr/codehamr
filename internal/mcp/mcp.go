// Package mcp is a minimal MCP (Model Context Protocol) client: spawns each
// server as a child process and speaks JSON-RPC 2.0 over stdio. One server
// per map entry, one mutex per server — requests serialize cleanly.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
)

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc %d: %s", e.Code, e.Message) }

// Server represents one running MCP child.
type Server struct {
	Name string

	cmd   *exec.Cmd
	stdin io.WriteCloser
	scan  *bufio.Scanner

	mu    sync.Mutex // serializes send+read per server
	next  int
	tools []ToolDef
}

// spawnTimeout bounds the initialize + tools/list round-trip. npx first-run
// downloads can take seconds; a hung or misbehaving server must not wedge
// the spawn goroutine forever. On timeout we Stop() the child and surface
// the error so SpawnAll can log "<name>: handshake timeout" instead of
// leaking a zombie.
const spawnTimeout = 30 * time.Second

// spawn starts the child process and performs the initialize handshake. The
// caller owns lifecycle — use Stop to terminate. Unexported because external
// callers always go through Manager.Spawn, which adds the registry bookkeeping
// the bare child has no notion of.
func spawn(name string, mc config.MCPServer) (*Server, error) {
	cmd := exec.Command(mc.Command, mc.Args...)
	if len(mc.Env) > 0 {
		env := os.Environ()
		for k, v := range mc.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard // don't mix server noise into the TUI
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &Server{
		Name:  name,
		cmd:   cmd,
		stdin: stdin,
		scan:  bufio.NewScanner(stdout),
	}
	s.scan.Buffer(make([]byte, 1<<16), 4<<20)

	// Run handshake + listTools on a worker and race against a timer. On
	// timeout, Stop() the child: that closes stdin, SIGINTs the process,
	// and SIGKILLs on delay — which in turn closes stdout and lets the
	// worker's blocked Scan() return, so the goroutine exits cleanly.
	type result struct {
		tools []ToolDef
		err   error
	}
	done := make(chan result, 1)
	go func() {
		if err := s.handshake(); err != nil {
			done <- result{err: err}
			return
		}
		tools, err := s.listTools()
		done <- result{tools: tools, err: err}
	}()
	t := time.NewTimer(spawnTimeout)
	defer t.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			s.Stop()
			return nil, r.err
		}
		s.tools = r.tools
		return s, nil
	case <-t.C:
		s.Stop()
		return nil, fmt.Errorf("handshake timeout after %s", spawnTimeout)
	}
}

func (s *Server) handshake() error {
	if _, err := s.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "codehamr", "version": "0.1"},
	}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.stdin, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
}

func (s *Server) listTools() ([]ToolDef, error) {
	raw, err := s.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Tools, nil
}

// callResult carries a tools/call outcome back from the worker goroutine.
// Named so the five send sites in Call() read cleanly.
type callResult struct {
	out string
	err error
}

// Call invokes `tools/call` on this server.
func (s *Server) Call(parent context.Context, tool string, args map[string]any) (string, error) {
	done := make(chan callResult, 1)
	go func() {
		raw, err := s.call("tools/call", map[string]any{
			"name":      tool,
			"arguments": args,
		})
		if err != nil {
			done <- callResult{err: err}
			return
		}
		var r struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			done <- callResult{err: err}
			return
		}
		var b strings.Builder
		for _, c := range r.Content {
			b.WriteString(c.Text)
		}
		if r.IsError {
			done <- callResult{out: b.String(), err: errors.New("tool reported error")}
			return
		}
		done <- callResult{out: b.String()}
	}()
	select {
	case <-parent.Done():
		return "", parent.Err()
	case res := <-done:
		return res.out, res.err
	}
}

func (s *Server) Tools() []ToolDef { return s.tools }

// Stop terminates the child, SIGTERM then force-kill on timeout.
func (s *Server) Stop() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		_ = s.cmd.Process.Kill()
		<-done
	}
}

func (s *Server) call(method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := s.next
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := writeJSON(s.stdin, req); err != nil {
		return nil, err
	}
	for s.scan.Scan() {
		line := s.scan.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// skip unexpected server-initiated notifications
			continue
		}
		// id is always >= 1 (s.next starts at 0 and is pre-incremented),
		// so server-initiated notifications (id absent → 0) are filtered
		// here by the same mismatch check that handles late stragglers
		// from earlier RPCs.
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
	if err := s.scan.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// Manager coordinates every enabled MCP server and exposes the aggregated
// tool list to the LLM layer. All map access goes through mu because
// SpawnAll runs in a background goroutine (main.go) while the TUI goroutine
// concurrently reads the registry via Tools/Has/Call and may mutate it via
// Spawn/Stop from the /plugins toggle. Without the lock, Go's map type
// panics on concurrent read/write.
type Manager struct {
	mu        sync.Mutex
	servers   map[string]*Server
	toolIndex map[string]string // tool name → server name for O(1) dispatch
}

func NewManager() *Manager {
	return &Manager{servers: map[string]*Server{}, toolIndex: map[string]string{}}
}

// SpawnAll starts every server in `servers` whose Enabled is true. Errors
// are collected and reported; one bad server never blocks the others. The
// caller is expected to pass a snapshot — main.go takes one before
// launching SpawnAll on a goroutine so /plugins toggles in the TUI never
// race against this iteration on the underlying config map.
func (m *Manager) SpawnAll(servers map[string]config.MCPServer) []error {
	var errs []error
	for name, sc := range servers {
		if !sc.Enabled {
			continue
		}
		if err := m.Spawn(name, sc); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errs
}

// Spawn starts a single server and registers it. The child-process spawn
// itself runs outside the manager mutex because it blocks on the handshake
// (child fork + stdio round-trip, hundreds of ms at best, potentially
// seconds for npx first-run); holding the registry lock that long would
// serialize every Tools/Has/Call call on the TUI goroutine.
func (m *Manager) Spawn(name string, mc config.MCPServer) error {
	m.mu.Lock()
	if _, ok := m.servers[name]; ok {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	s, err := spawn(name, mc)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Another goroutine may have raced us to register the same name; if so,
	// discard ours and stop the extra child rather than leaving it orphaned.
	if _, ok := m.servers[name]; ok {
		go s.Stop()
		return nil
	}
	m.servers[name] = s
	for _, t := range s.Tools() {
		m.toolIndex[t.Name] = name
	}
	return nil
}

func (m *Manager) Stop(name string) {
	m.mu.Lock()
	s, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	for _, t := range s.Tools() {
		delete(m.toolIndex, t.Name)
	}
	delete(m.servers, name)
	m.mu.Unlock()
	s.Stop() // Stop blocks on process wait; release lock first.
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	servers := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	m.servers = map[string]*Server{}
	m.toolIndex = map[string]string{}
	m.mu.Unlock()
	for _, s := range servers {
		s.Stop()
	}
}

// Tools returns the flat tool list to hand to the LLM. Servers are walked
// in sorted name order so the resulting tool slice is reproducible across
// calls — Go map iteration is random, and a non-deterministic tool order
// makes the chat payload (and prompt-cache key) shift between turns for
// no semantic reason.
func (m *Manager) Tools() []llm.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []llm.Tool
	for _, n := range slices.Sorted(maps.Keys(m.servers)) {
		for _, t := range m.servers[n].Tools() {
			out = append(out, llm.Tool{
				Type: "function",
				Function: llm.FunctionDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}
	}
	return out
}

// Has implements tools.MCPDispatcher.
func (m *Manager) Has(tool string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.toolIndex[tool]
	return s, ok
}

// Call implements tools.MCPDispatcher.
func (m *Manager) Call(ctx context.Context, server, tool string, args map[string]any) (string, error) {
	m.mu.Lock()
	s, ok := m.servers[server]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("mcp: server %q not running", server)
	}
	return s.Call(ctx, tool, args)
}
