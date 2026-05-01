package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/cloud"
	"github.com/codehamr/codehamr/internal/llm"
)

// probeTimeout caps the activation hello-world request. Long enough that a
// cold cloud route can finish, short enough that a stuck backend doesn't
// leave the user staring at "▶ probing" forever.
const probeTimeout = 15 * time.Second

// probeMsg carries the outcome of a one-off Probe (hello-world chat) used
// at activation time to validate URL+model+key in one round trip and harvest
// the live context window from the response headers. profile is the name
// the activation was targeted at — recorded explicitly because the user
// could /models switch again before the probe returns, and we don't want a
// late probe to overwrite the wrong profile's live window.
type probeMsg struct {
	profile       string
	contextWindow int
	budget        cloud.BudgetStatus
	silent        bool // suppress the "✓ active" line — startup probe only
	err           error
}

// probeBackend wraps llm.Client.Probe in a tea.Cmd. Bounded by probeTimeout
// so a hung backend never freezes the activation flow. silent=true skips
// the "✓ active" scrollback line — used by the startup probe so it only
// initialises the live budget/ctx values without echoing an activation
// banner the user didn't ask for.
func probeBackend(cli *llm.Client, profileName string, silent bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		res, err := cli.Probe(ctx)
		return probeMsg{
			profile:       profileName,
			contextWindow: res.ContextWindow,
			budget:        res.Budget,
			silent:        silent,
			err:           err,
		}
	}
}

// handleProbe consumes the result of an activation-time Probe. On success
// it stores the live context window for the targeted profile and prints
// the final activation line with the live ctx suffix. On failure it
// surfaces the error inline (key rejected, unreachable, etc.) and leaves
// the active profile as set — the user can /models back if they want.
// Late probes whose profile is no longer active still update
// liveContextSize so the value is ready next time the user switches back.
func (m Model) handleProbe(msg probeMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.connected = false
		// Silent startup probes don't print activation banners on success,
		// so they shouldn't print error banners on failure either —
		// otherwise an offline launch greets the user with a noisy "⚠ probe"
		// line before they've done anything. The connected=false alone is
		// enough; the next user action will surface the real failure.
		if !msg.silent {
			m.appendLine(styleError.Render("⚠ probe " + msg.profile + ": " + probeErrorMessage(msg.err)))
		}
		return m, nil
	}
	m.connected = true
	if msg.contextWindow > 0 {
		m.liveContextSize[msg.profile] = msg.contextWindow
	}
	if msg.budget.Set && msg.profile == m.cfg.Active {
		m.budget = msg.budget
	}
	p, ok := m.cfg.Models[msg.profile]
	if !ok {
		// Profile vanished between probe dispatch and return (user hand-
		// edited config). Nothing meaningful to print; the live window is
		// still cached in case the profile reappears.
		return m, nil
	}
	if msg.silent {
		return m, nil
	}
	suffix := ""
	if msg.contextWindow > 0 {
		suffix = fmt.Sprintf(" · ctx: %s", humanInt(msg.contextWindow))
	}
	m.appendLine(styleOK.Render(fmt.Sprintf(
		"✓ active: %s · %s @ %s%s", msg.profile, p.LLM, p.URL, suffix)))
	return m, nil
}

// probeErrorMessage maps the cloud sentinel errors to human strings so the
// activation line carries a useful hint instead of a stack-trace-style
// wrap. Falls back to the raw error string for anything unrecognised.
func probeErrorMessage(err error) string {
	switch {
	case errors.Is(err, cloud.ErrUnauthorized):
		return "key rejected"
	case errors.Is(err, cloud.ErrBudgetExhausted):
		return "budget exhausted"
	}
	if un, ok := errors.AsType[cloud.ErrUnreachable](err); ok {
		return "unreachable (" + un.Err.Error() + ")"
	}
	return err.Error()
}
