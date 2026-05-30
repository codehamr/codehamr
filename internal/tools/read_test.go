package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestReadFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	content := "line one\nline two with 'quotes' and $dollar and `backticks`\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path)
	if got != content {
		t.Fatalf("read content mismatch:\n got %q\nwant %q", got, content)
	}
}

func TestReadFileEmptyPath(t *testing.T) {
	if got := ReadFile(""); got != "(empty path)" {
		t.Fatalf("empty path handling wrong: %q", got)
	}
}

func TestReadFileMissingFile(t *testing.T) {
	s := ReadFile(filepath.Join(t.TempDir(), "nope.txt"))
	if !strings.HasPrefix(s, "(read error:") {
		t.Fatalf("expected (read error: ...) string, got %q", s)
	}
}

// TestReadFileTruncatesOversizeContent: ReadFile obeys the same Truncate
// head+tail cap as every other tool. Oversize content must carry the marker so
// the model re-reads a slice rather than trusting a silently-clipped blob.
func TestReadFileTruncatesOversizeContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// Over ToolOutputCap*4 bytes so Truncate fires.
	big := strings.Repeat("x", chmctx.ToolOutputCap*4+1000)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path)
	if !strings.Contains(got, "───── truncated") {
		t.Fatalf("oversize read should carry the truncation marker, got %d bytes without it", len(got))
	}
	if len(got) >= len(big) {
		t.Fatalf("oversize read was not shortened: got %d bytes, source %d", len(got), len(big))
	}
}

func TestReadFileSchemaShape(t *testing.T) {
	sch := ReadFileSchema()
	fn, ok := sch["function"].(map[string]any)
	if !ok {
		t.Fatal("missing function")
	}
	if fn["name"] != "read_file" {
		t.Fatalf("name wrong: %v", fn["name"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatal("missing parameters")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props["path"]; !ok {
		t.Fatal("missing property \"path\"")
	}
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "path" {
		t.Fatalf("required should be [\"path\"], got %v", params["required"])
	}
}

func TestExecuteReadFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.txt")
	content := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	call := chmctx.ToolCall{
		ID:        "call_r",
		Name:      ReadFileName,
		Arguments: map[string]any{"path": path},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_r" || msg.ToolName != ReadFileName {
		t.Fatalf("bad message: %+v", msg)
	}
	if msg.Content != content {
		t.Fatalf("content mismatch:\n got %q\nwant %q", msg.Content, content)
	}
}

func TestInlineStatusReadFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name:      ReadFileName,
		Arguments: map[string]any{"path": "/tmp/foo.go"},
	})
	if !strings.HasPrefix(s, "▶ read_file: /tmp/foo.go") {
		t.Fatalf("bad inline status: %q", s)
	}
}
