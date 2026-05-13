package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestEditFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("alpha beta gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "beta", "BRAVO")
	if !strings.HasPrefix(s, "edited") {
		t.Fatalf("status wrong: %q", s)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "alpha BRAVO gamma\n" {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestEditFileEmptyPath(t *testing.T) {
	if got := EditFile("", "x", "y"); got != "(empty path)" {
		t.Fatalf("bad: %q", got)
	}
}

func TestEditFileMissingFile(t *testing.T) {
	s := EditFile(filepath.Join(t.TempDir(), "nope.txt"), "x", "y")
	if !strings.HasPrefix(s, "(read error:") {
		t.Fatalf("bad: %q", s)
	}
}

func TestEditFileOldNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "missing", "x")
	if !strings.Contains(s, "not found") {
		t.Fatalf("bad: %q", s)
	}
	// File untouched.
	got, _ := os.ReadFile(path)
	if string(got) != "abc" {
		t.Fatalf("file modified on miss: %q", got)
	}
}

func TestEditFileOldNotUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "foo", "qux")
	if !strings.Contains(s, "appears") || !strings.Contains(s, "2") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo bar foo" {
		t.Fatalf("file modified on ambiguity: %q", got)
	}
}

func TestEditFileNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "x", "x")
	if !strings.Contains(s, "no change") {
		t.Fatalf("bad: %q", s)
	}
}

func TestEditFileEmptyOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "", "x")
	if !strings.Contains(s, "empty") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "abc" {
		t.Fatalf("file modified on empty old_string: %q", got)
	}
}

func TestEditFileDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "beta ", "")
	if !strings.HasPrefix(s, "edited") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha gamma" {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestEditFileMultilineOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	src := "func foo() {\n\treturn 1\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "\treturn 1\n", "\treturn 42\n")
	if !strings.HasPrefix(s, "edited") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "func foo() {\n\treturn 42\n}\n" {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestExecuteEditFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := chmctx.ToolCall{
		ID:   "call_e",
		Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": "world",
			"new_string": "earth",
		},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_e" || msg.ToolName != "edit_file" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.HasPrefix(msg.Content, "edited") {
		t.Fatalf("content missing: %q", msg.Content)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello earth" {
		t.Fatalf("file content wrong: %q", got)
	}
}

func TestInlineStatusEditFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name: "edit_file",
		Arguments: map[string]any{
			"path":       "/tmp/x.txt",
			"old_string": "a",
			"new_string": "b",
		},
	})
	if !strings.HasPrefix(s, "▶ edit_file: /tmp/x.txt") {
		t.Fatalf("bad inline status: %q", s)
	}
}

func TestEditFileSchemaShape(t *testing.T) {
	sch := EditFileSchema()
	fn, ok := sch["function"].(map[string]any)
	if !ok {
		t.Fatal("missing function")
	}
	if fn["name"] != "edit_file" {
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
	for _, key := range []string{"path", "old_string", "new_string"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("missing property %q", key)
		}
	}
}
