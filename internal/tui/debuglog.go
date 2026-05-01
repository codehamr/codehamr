// Debug instrumentation. The whole file plus its four call sites
// (search for `dbgWrite`) are intentionally self-contained so this can
// be ripped out cleanly when no longer needed. Activated by `logging:
// true` in .codehamr/config.yaml; .codehamr/log.txt is truncated on
// every start so a session never appends onto a stale run.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

var (
	dbgMu   sync.Mutex
	dbgFile *os.File
)

// OpenDebugLog truncates <dir>/log.txt and opens it for writing. A failure
// is reported once on stderr and silently disables logging for the rest of
// the run — the debug log must never block the TUI from starting.
func OpenDebugLog(dir string) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, "log.txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "⚠ debuglog:", err)
		return
	}
	dbgMu.Lock()
	dbgFile = f
	dbgMu.Unlock()
	dbgWritef("session", "codehamr started · project=%s", dir)
}

// CloseDebugLog flushes and closes the log. Idempotent.
func CloseDebugLog() {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile != nil {
		_ = dbgFile.Close()
		dbgFile = nil
	}
}

// dbgWritef appends one timestamped record. No-op when logging is off.
func dbgWritef(category, format string, args ...any) {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile == nil {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	body := fmt.Sprintf(format, args...)
	fmt.Fprintf(dbgFile, "[%s] %s\n%s\n\n", ts, category, body)
}

// dbgWriteMessage records a chmctx.Message in a human readable shape:
// thinking, content, and tool calls each get their own labeled section.
// No-op when logging is off, so callers don't need to guard.
func dbgWriteMessage(category string, msg chmctx.Message) {
	dbgMu.Lock()
	enabled := dbgFile != nil
	dbgMu.Unlock()
	if !enabled {
		return
	}
	var b strings.Builder
	if msg.Thinking != "" {
		b.WriteString("THINKING:\n")
		b.WriteString(msg.Thinking)
		b.WriteString("\n")
	}
	if msg.Content != "" {
		b.WriteString("CONTENT:\n")
		b.WriteString(msg.Content)
		b.WriteString("\n")
	}
	for _, tc := range msg.ToolCalls {
		args, _ := json.Marshal(tc.Arguments)
		fmt.Fprintf(&b, "TOOL_CALL %s id=%s args=%s\n", tc.Name, tc.ID, args)
	}
	if msg.ToolCallID != "" {
		fmt.Fprintf(&b, "tool=%s id=%s\n", msg.ToolName, msg.ToolCallID)
	}
	dbgWritef(category, "%s", strings.TrimRight(b.String(), "\n"))
}
