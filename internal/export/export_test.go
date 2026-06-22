package export

import (
	"os"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestToHTMLBasicConversation(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "hello world"},
		{Role: chmctx.RoleAssistant, Content: "hi there"},
	}
	html, err := ToHTML(history, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "hello world") {
		t.Error("missing user content")
	}
	if !strings.Contains(html, "hi there") {
		t.Error("missing assistant content")
	}
	if !strings.Contains(html, `<div class="role user">user</div>`) {
		t.Error("missing user role label")
	}
	if !strings.Contains(html, `<div class="role assistant">assistant</div>`) {
		t.Error("missing assistant role label")
	}
}

func TestToHTMLSystemPromptPrepended(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "hi"},
	}
	html, err := ToHTML(history, "you are codehamr")
	if err != nil {
		t.Fatal(err)
	}
	sysIdx := strings.Index(html, "you are codehamr")
	userIdx := strings.Index(html, ">hi<")
	if sysIdx < 0 || userIdx < 0 {
		t.Fatal("missing system or user content")
	}
	if sysIdx > userIdx {
		t.Error("system prompt should appear before user message")
	}
}

func TestToHTMLEmptySystemNotIncluded(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "hi"},
	}
	html, err := ToHTML(history, "   ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "class=\"msg system\"") {
		t.Error("empty system prompt should not produce a system block")
	}
}

func TestToHTMLCodeBlock(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleAssistant, Content: "here:\n```go\nfmt.Println(\"hi\")\n```\ndone"},
	}
	html, err := ToHTML(history, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "<pre><code>") {
		t.Error("missing <pre><code> for fenced block")
	}
	if !strings.Contains(html, "fmt.Println") {
		t.Error("code content lost")
	}
	// The code inside must be escaped, not raw.
	if strings.Contains(html, "fmt.Println(\"hi\")") {
		t.Error("code block content should be HTML-escaped")
	}
}

func TestToHTMLToolCallAndResult(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleAssistant, Content: "let me read that", ToolCalls: []chmctx.ToolCall{
			{ID: "tc1", Name: "read_file", Arguments: map[string]any{"path": "/tmp/x.go"}},
		}},
		{Role: chmctx.RoleTool, ToolCallID: "tc1", ToolName: "read_file", Content: "package main"},
	}
	html, err := ToHTML(history, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "tool call: read_file") {
		t.Error("missing tool call summary")
	}
	if !strings.Contains(html, "path: /tmp/x.go") {
		t.Error("missing tool call args")
	}
	if !strings.Contains(html, "package main") {
		t.Error("missing tool result content")
	}
	if !strings.Contains(html, `class="msg tool"`) {
		t.Error("missing tool message class")
	}
}

func TestToHTMLErrorDetection(t *testing.T) {
	cases := []struct {
		content string
		wantErr bool
	}{
		{"command not found (exit: 127)", true},
		{"all good (exit: 0)", false},
		{"error: something broke", true},
		{"Error: nil pointer", true},
		{"normal output", false},
	}
	for _, c := range cases {
		got := looksLikeError(c.content)
		if got != c.wantErr {
			t.Errorf("looksLikeError(%q) = %v, want %v", c.content, got, c.wantErr)
		}
	}
}

func TestToHTMLEscaping(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleUser, Content: `<script>alert("xss")</script>`},
	}
	html, err := ToHTML(history, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<script>alert") {
		t.Error("user content not escaped - XSS risk")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("expected escaped script tag")
	}
}

func TestToFileWritesHTML(t *testing.T) {
	history := []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "test message"},
	}
	path := t.TempDir() + "/session.html"
	if err := ToFile(history, "sys prompt", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "test message") {
		t.Error("file missing content")
	}
	if !strings.Contains(string(data), "<!DOCTYPE html>") {
		t.Error("file missing DOCTYPE")
	}
}

func TestToHTMLEmptyHistory(t *testing.T) {
	html, err := ToHTML(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("empty history should still produce valid HTML")
	}
}
