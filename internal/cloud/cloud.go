// Package cloud parses the budget and auth signals from the codehamr.com proxy.
// Client-side plumbing only; the server owns all accounting.
//
// Wire contract: one budget-fraction header (0.0..1.0), one context-window
// header, plus standard 401/402. A hamrpass is a prepaid pot of budget — no
// cooldowns, rate limits, resets, or expiry.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
)

// Reachable GETs baseURL/v1/models with ctx as deadline; any HTTP response
// (even 401/404) counts as reachable, only transport errors/timeouts don't.
// Backs the TUI connectivity probe for keyless (local) profiles.
//
// /v1/models is the route-registered OpenAI heartbeat every keyless backend
// serves (Ollama, vLLM, lmstudio, llama.cpp). A root GET / hangs on vLLM,
// which has no root route and blocks behind the inference loop — probe must
// hit a real route to return promptly.
//
// Drain the body before close so the TCP connection returns to the pool;
// closing undrained leaks it in keep-alive setups.
func Reachable(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

// Headers the proxy sets on every 200. Remaining is the pass fraction
// [0.0,1.0]; CtxWindow is the live context window, authoritative over config.yaml.
const (
	headerRemaining  = "X-Budget-Remaining"
	headerCtxWindow  = "X-Context-Window"
	ctxWindowMin     = 1024
	ctxWindowMaxSane = 8 * 1024 * 1024 // 8M tokens; larger is a bug, not a config
)

// BudgetStatus is the client's latest snapshot of server accounting. Set is
// false until the first cloud response, so zero values never render. Remaining
// is the pass fraction (1.0 = fresh, 0.0 = depleted).
type BudgetStatus struct {
	Set       bool
	Remaining float64
}

// FromHeaders reads the budget header. Missing (e.g. local Ollama) yields the
// zero value and the UI skips the segment. Out-of-range values are clamped, not
// rejected, so server-side rounding past 1.0 doesn't blank the readout.
//
// NaN/±Inf are rejected: NaN slips past the clamp (all comparisons false), and
// the UI's int(NaN*100+0.5) yields MinInt64 → a garbage percentage. Both are
// treated as "no signal", like a missing header.
func FromHeaders(h http.Header) BudgetStatus {
	raw := h.Get(headerRemaining)
	if raw == "" {
		return BudgetStatus{}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return BudgetStatus{}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return BudgetStatus{}
	}
	switch {
	case v < 0:
		v = 0
	case v > 1:
		v = 1
	}
	return BudgetStatus{Set: true, Remaining: v}
}

// StatusSuffix builds the " · 73% pass" status-bar tail; "" before the first
// snapshot. Rounded to nearest percent so it doesn't jitter on every token.
func (b BudgetStatus) StatusSuffix() string {
	if !b.Set {
		return ""
	}
	return fmt.Sprintf(" · %d%% pass", int(b.Remaining*100+0.5))
}

// ErrBudgetExhausted maps server 402: the pass is depleted and the user must
// top up before any further request succeeds.
var ErrBudgetExhausted = errors.New("hamrpass depleted")

// ErrUnauthorized maps server 401.
var ErrUnauthorized = errors.New("invalid or expired token")

// ErrUnreachable wraps transport errors (refused, timeout, DNS miss) so the TUI
// can render a hint instead of a raw wrap.
type ErrUnreachable struct{ Err error }

func (e ErrUnreachable) Error() string { return "backend unreachable: " + e.Err.Error() }
func (e ErrUnreachable) Unwrap() error { return e.Err }

// AuthHeader returns the Bearer header a cloud-routed request needs.
func AuthHeader(token string) string { return "Bearer " + token }

// ContextWindowFromHeaders reads X-Context-Window. Returns 0 when missing,
// malformed, or out of sane range; the caller reads 0 as "use the fallback".
// Local Ollama never sets it, so it keeps its config.yaml value.
func ContextWindowFromHeaders(h http.Header) int {
	raw := h.Get(headerCtxWindow)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < ctxWindowMin || n > ctxWindowMaxSane {
		return 0
	}
	return n
}
