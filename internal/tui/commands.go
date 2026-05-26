package tui

import (
	"context"
	"errors"

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

// toolResultMsg carries one finished tool call back to Update. turnCtx is
// the per-turn context the call was dispatched against; Update drops the
// message when that ctx no longer matches m.turnCtx — i.e. the user has
// already Ctrl+C'd the originating turn and (possibly) submitted a new
// one. Without this guard the orphan tool result would be appended to the
// new turn's history (its tool_call_id has no preceding assistant.tool_calls
// in the live conversation) and immediately re-enter chat with startChat,
// abandoning the legitimate stream that turn N+1 had just started.
type toolResultMsg struct {
	Msg     chmctx.Message
	turnCtx context.Context
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
// is the per-turn root; a Ctrl+C cancel of the turn aborts the tool mid-run,
// and the returned toolResultMsg carries that ctx so Update can drop the
// message when it lands on a turn that has since moved on.
//
// No outer timeout is wrapped here — bash, write_file, and edit_file own their
// own per-call timeouts (bash defaults to 2 min, capped at 3600s by the schema;
// write_file and edit_file are filesystem-fast and use no timeout). Wrapping in a hardcoded
// outer cap would silently override the model-set bash timeout, so a request
// for a 30-min docker build would die at 3 min with a confusing "timeout"
// inside an hour-long apparent allowance.
func runToolCall(parent context.Context, call chmctx.ToolCall) tea.Cmd {
	return func() tea.Msg {
		return toolResultMsg{Msg: tools.Execute(parent, call), turnCtx: parent}
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
