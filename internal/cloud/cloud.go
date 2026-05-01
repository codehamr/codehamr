// Package cloud parses the budget and auth signals exchanged with the cloud
// proxy at codehamr.com. Everything here is client-side plumbing;
// the server owns all accounting logic.
//
// The wire contract is intentionally minimal: one budget header (a fraction
// 0.0..1.0), one context-window header, plus the standard 401 and 402 status
// codes. No cooldowns, no rate limiting, no resets, no expiry dates. A
// hamrpass is a prepaid pot of budget, full stop.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Reachable does a GET against baseURL with ctx as its deadline. Any HTTP
// response, even 401 or 404, counts as reachable; only transport errors
// and timeouts return non-nil. Used by the TUI's live connectivity probe.
func Reachable(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// Response headers the cloud proxy sets on every 200. headerRemaining is the
// fraction of the prepaid pass still available, expressed as a float in
// [0.0, 1.0]. headerCtxWindow is the live context window the server allocates
// for this caller, authoritative over any value the client has in config.yaml.
const (
	headerRemaining  = "X-Budget-Remaining"
	headerCtxWindow  = "X-Context-Window"
	ctxWindowMin     = 1024
	ctxWindowMaxSane = 8 * 1024 * 1024 // 8M tokens, anything larger is a bug, not a config
)

// BudgetStatus is the latest snapshot the client has of the server's
// accounting. Set is false until the first cloud response is parsed, so zero
// values never render in the UI. Remaining is the fraction of the pass still
// available (1.0 = fresh, 0.0 = depleted).
type BudgetStatus struct {
	Set       bool
	Remaining float64
}

// FromHeaders reads the budget header. Local Ollama responses don't carry
// it; in that case the zero value is returned and the UI skips the budget
// segment. Out-of-range values are clamped rather than rejected because a
// brief over-shoot above 1.0 (server-side rounding) shouldn't blank the
// status segment.
func FromHeaders(h http.Header) BudgetStatus {
	raw := h.Get(headerRemaining)
	if raw == "" {
		return BudgetStatus{}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
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

// StatusSuffix builds the " · 73% pass" tail shown in the TUI's bottom
// status bar. Returns "" before the first snapshot. Rounded to the nearest
// percent so the readout doesn't jitter on every token.
func (b BudgetStatus) StatusSuffix() string {
	if !b.Set {
		return ""
	}
	return fmt.Sprintf(" · %d%% pass", int(b.Remaining*100+0.5))
}

// ErrBudgetExhausted is returned when the server responds 402 Payment Required.
// The pass is depleted and the user has to top up before any further request
// will succeed.
var ErrBudgetExhausted = errors.New("hamrpass depleted")

// ErrUnauthorized is returned when the server responds 401.
var ErrUnauthorized = errors.New("invalid or expired token")

// ErrUnreachable wraps transport errors (connection refused, timeout, DNS
// miss) so the TUI can render a useful hint instead of a stack-trace-style
// wrap.
type ErrUnreachable struct{ Err error }

func (e ErrUnreachable) Error() string { return "backend unreachable: " + e.Err.Error() }
func (e ErrUnreachable) Unwrap() error { return e.Err }

// AuthHeader returns the Bearer header a cloud-routed request needs.
func AuthHeader(token string) string { return "Bearer " + token }

// ContextWindowFromHeaders reads X-Context-Window. Returns 0 when the header
// is missing, malformed, or outside a sane range. The caller treats 0 as
// "no live value, use whatever fallback you have". Local Ollama responses
// don't set this header, so they always get 0 and keep their config.yaml
// value.
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
