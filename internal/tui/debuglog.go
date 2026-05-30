// Debug instrumentation, self-contained so it can be ripped out cleanly.
// Activated by `logging: true` in config.yaml; log.txt is truncated on
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

// OpenDebugLog truncates <dir>/log.txt and opens it for writing. On failure
// it reports once on stderr and disables logging — the log must never block
// the TUI from starting.
//
// 0o600 because the log captures every prompt: /hamrpass <key> and bash args
// can carry secrets even past the slash redaction below. Owner-only is the
// only honest answer.
func OpenDebugLog(dir string) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, "log.txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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

// redactSlash strips the /hamrpass <key> bearer token before it lands in any
// log. A central hook covers any future secret-bearing command from one place.
//
// The split mirrors runSlash's strings.Fields — both must agree on command vs.
// args, else a multi-line `/hamrpass\n<key>` (Alt+Enter inserts a literal
// newline) activates via runSlash but slips a literal-space prefix match here,
// leaking the verbatim key.
//
// Case-folded on purpose: a mistyped /HamrPass won't activate (dispatch is
// case-sensitive) but its token would still reach scrollback, recall ring,
// history, and log.txt — so redaction errs wider than dispatch, the safe way.
func redactSlash(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "/hamrpass") {
		return line
	}
	if len(fields) == 1 {
		return line // no key portion to redact
	}
	return "/hamrpass <redacted>"
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

// dbgWriteMessage records a chmctx.Message readably: content and tool calls
// each get a labeled section. No-op when logging is off, so callers needn't guard.
func dbgWriteMessage(category string, msg chmctx.Message) {
	dbgMu.Lock()
	enabled := dbgFile != nil
	dbgMu.Unlock()
	if !enabled {
		return
	}
	var b strings.Builder
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
