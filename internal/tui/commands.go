package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/cloud"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
)

// streamEventMsg and streamClosedMsg carry their originating channel so the
// model can drop messages produced by a stream that the current turn no
// longer owns. After Ctrl+C → fresh submit, the prior turn's readEvent Cmd is
// still scheduled and its eventual emit can land on the new turn; without
// the ch tag we'd write its tokens into the new turn's buffers, or worse,
// let its close event run handleStreamClosed → endTurn against the new turn.
type streamEventMsg struct {
	ch <-chan llm.Event
	e  llm.Event
}

type streamClosedMsg struct {
	ch <-chan llm.Event
}

type toolResultMsg struct {
	Msg chmctx.Message
}

// readEvent drains one event from the LLM stream and returns it as a tea.Msg.
// It's re-scheduled after each update until the channel closes. The msg
// carries ch back so the dispatcher in Update can spot stale messages from
// abandoned prior turns.
func readEvent(ch <-chan llm.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return streamClosedMsg{ch: ch}
		}
		return streamEventMsg{ch: ch, e: e}
	}
}

// runToolCall executes one tool call off the UI goroutine. The parent context
// is the per-turn root; a Ctrl+C cancel of the turn aborts the tool mid-run.
func runToolCall(parent context.Context, call chmctx.ToolCall, disp tools.MCPDispatcher) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
		defer cancel()
		return toolResultMsg{Msg: tools.Execute(ctx, call, disp)}
	}
}

// errorMessage maps a stream error into the one-line TUI hint. One format
// across all profiles, no mode-specific branching.
func (m Model) errorMessage(e llm.Event) string {
	if e.Err == nil {
		return ""
	}
	switch {
	case errors.Is(e.Err, cloud.ErrBudgetExhausted):
		return "⚠ hamrpass depleted · top up at codehamr.com"
	case errors.Is(e.Err, cloud.ErrUnauthorized):
		return "⚠ key rejected · check models." + m.cfg.Active + ".key in .codehamr/config.yaml"
	case isUnreachable(e.Err):
		return "⚠ unreachable: " + m.cfg.ActiveURL() + " · /models to switch profile"
	default:
		return "⚠ " + e.Err.Error()
	}
}

func isUnreachable(err error) bool {
	_, ok := errors.AsType[cloud.ErrUnreachable](err)
	return ok
}
