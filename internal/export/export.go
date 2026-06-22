// Package export renders a conversation to a self-contained HTML file for the
// /share command. No external JS or CSS: messages are server-rendered to plain
// HTML with Go's html/template, code blocks in <pre><code>, and tool calls /
// results as collapsible <details>.
package export

import (
	"fmt"
	"html/template"
	"os"
	"strings"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// row is one rendered message in the transcript.
type row struct {
	Role        string
	Content     template.HTML
	ToolCalls   []toolCallRow
	ToolCallID  string
	ToolName    string
	IsTool      bool
	IsError     bool
}

type toolCallRow struct {
	Name string
	Args string
}

const tpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>codehamr session</title>
<style>
  :root {
    --bg: #1e1e2e;
    --card: #282839;
    --user: #313142;
    --asst: #242434;
    --tool: #1c1c2a;
    --accent: #e0a060;
    --text: #d4d4e0;
    --dim: #8888a0;
    --err: #f07070;
  }
  * { box-sizing: border-box; }
  body {
    background: var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    margin: 0;
    padding: 24px;
    line-height: 1.5;
  }
  .container { max-width: 920px; margin: 0 auto; }
  h1 { color: var(--accent); font-size: 1.1em; font-weight: 600; margin: 0 0 20px; }
  .msg {
    background: var(--card);
    border-radius: 8px;
    padding: 14px 18px;
    margin-bottom: 12px;
  }
  .msg.user { background: var(--user); }
  .msg.assistant { background: var(--asst); }
  .msg.tool { background: var(--tool); }
  .role {
    font-size: 0.75em;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    margin-bottom: 8px;
  }
  .role.user { color: var(--accent); }
  .role.assistant { color: var(--dim); }
  .role.tool { color: var(--dim); }
  .content { white-space: pre-wrap; word-wrap: break-word; }
  .content pre {
    background: #16161e;
    border-radius: 6px;
    padding: 12px;
    overflow-x: auto;
    font-size: 0.9em;
  }
  .content code { font-family: "SF Mono", "Fira Code", Consolas, monospace; }
  .content p { margin: 0.5em 0; }
  details {
    margin-top: 8px;
    border: 1px solid #3a3a4e;
    border-radius: 6px;
    padding: 8px 12px;
  }
  summary {
    cursor: pointer;
    color: var(--accent);
    font-size: 0.85em;
    font-weight: 600;
  }
  .tool-args {
    margin-top: 6px;
    white-space: pre-wrap;
    font-size: 0.85em;
    color: var(--dim);
  }
  .error { color: var(--err); }
  a { color: var(--accent); }
</style>
</head>
<body>
<div class="container">
  <h1>codehamr session</h1>
  {{range .}}
  <div class="msg {{.Role}}">
    {{if .IsTool}}
      <div class="role tool">tool{{if .ToolName}} · {{.ToolName}}{{end}}{{if .IsError}} <span class="error">error</span>{{end}}</div>
      <div class="content">{{.Content}}</div>
    {{else}}
      <div class="role {{.Role}}">{{.Role}}</div>
      <div class="content">{{.Content}}</div>
      {{range .ToolCalls}}
      <details>
        <summary>tool call: {{.Name}}</summary>
        <div class="tool-args">{{.Args}}</div>
      </details>
      {{end}}
    {{end}}
  </div>
  {{end}}
</div>
</body>
</html>`

// ToHTML renders the conversation history (plus optional system prompt) to a
// self-contained HTML string. The system prompt, if non-empty, is prepended as
// a dimmed system block so the viewer sees the full context the model had.
func ToHTML(history []chmctx.Message, systemPrompt string) (string, error) {
	rows := make([]row, 0, len(history)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		rows = append(rows, row{
			Role:    "system",
			Content: renderContent(systemPrompt),
		})
	}
	for _, m := range history {
		r := row{
			Role:       string(m.Role),
			Content:    renderContent(m.Content),
			IsTool:     m.Role == chmctx.RoleTool,
			ToolCallID: m.ToolCallID,
			ToolName:   m.ToolName,
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				r.ToolCalls = append(r.ToolCalls, toolCallRow{
					Name: tc.Name,
					Args: formatArgs(tc.Arguments),
				})
			}
		}
		// Detect error tool results: content starting with the conventional
		// "(exit: N)" non-zero marker from the bash tool, or "error:" prefix.
		if r.IsTool {
			r.IsError = looksLikeError(m.Content)
		}
		rows = append(rows, r)
	}

	t, err := template.New("session").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, rows); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// ToFile writes the rendered HTML to outPath.
func ToFile(history []chmctx.Message, systemPrompt, outPath string) error {
	html, err := ToHTML(history, systemPrompt)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(html), 0o644)
}

// renderContent converts a message body to safe HTML. Markdown is not parsed
// (keeping the package dependency-free); text is escaped and wrapped so
// pre-formatted blocks and plain prose both render readably. Triple-backtick
// fenced blocks become <pre><code>.
func renderContent(s string) template.HTML {
	var out strings.Builder
	parts := strings.Split(s, "```")
	for i, part := range parts {
		if i%2 == 1 {
			// fenced code block; strip a trailing newline from the fence
			code := strings.TrimPrefix(part, "\n")
			out.WriteString("<pre><code>")
			out.WriteString(template.HTMLEscapeString(code))
			out.WriteString("</code></pre>")
			continue
		}
		// prose: escape, convert blank lines to <p> breaks
		paragraphs := strings.Split(part, "\n\n")
		for j, p := range paragraphs {
			if j > 0 {
				out.WriteString("<br><br>")
			}
			escaped := template.HTMLEscapeString(p)
			// single newlines within a paragraph -> <br>
			escaped = strings.ReplaceAll(escaped, "\n", "<br>")
			out.WriteString(escaped)
		}
	}
	return template.HTML(out.String())
}

// formatArgs renders a tool-call argument map as a readable key=value list.
func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range args {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(fmt.Sprint(v))
	}
	return b.String()
}

// looksLikeError heuristically flags tool results that represent failures so
// the HTML can mark them red. Matches the bash tool's "(exit: N)" non-zero
// convention and a leading "error:".
func looksLikeError(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "error:") || strings.HasPrefix(s, "Error:") {
		return true
	}
	if i := strings.LastIndex(s, "(exit:"); i >= 0 {
		tail := s[i:]
		// "(exit: 0)" is success; any other code is an error.
		return !strings.Contains(tail, "(exit: 0)")
	}
	return false
}
