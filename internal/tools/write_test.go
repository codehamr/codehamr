package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestWriteFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	content := "line one\nline two with 'quotes' and $dollar and `backticks`\n"
	s := WriteFile(path, content)
	if !strings.Contains(s, "wrote") || !strings.Contains(s, "hello.txt") {
		t.Fatalf("status wrong: %q", s)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestWriteFileCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "file.txt")
	s := WriteFile(path, "x")
	if !strings.Contains(s, "wrote 1 bytes") {
		t.Fatalf("status wrong: %q", s)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestWriteFileEmptyPath(t *testing.T) {
	if WriteFile("", "x") != "(empty path)" {
		t.Fatal("empty path handling wrong")
	}
}

func TestExecuteWriteFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	call := chmctx.ToolCall{
		ID:   "call_w",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    path,
			"content": "hello",
		},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_w" || msg.ToolName != "write_file" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.Contains(msg.Content, "wrote 5 bytes") {
		t.Fatalf("content missing: %q", msg.Content)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("file content wrong: %q", got)
	}
}

func TestInlineStatusWriteFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name:      "write_file",
		Arguments: map[string]any{"path": "/tmp/foo.txt", "content": "x"},
	})
	if !strings.HasPrefix(s, "▶ write_file: /tmp/foo.txt") {
		t.Fatalf("bad inline status: %q", s)
	}
}
