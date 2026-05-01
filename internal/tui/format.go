package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codehamr/codehamr/internal/config"
)

// humanTokens renders a token count compactly: `900 tok`, `1.2k tok`,
// `42k tok`, `1.5M tok`. Trims a trailing `.0` so round multiples read
// cleanly as `1k`, `10M`. Minimal, no external deps, one function.
func humanTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d tok", n)
	case n < 1_000_000:
		return compactFloat(float64(n)/1000) + "k tok"
	default:
		return compactFloat(float64(n)/1_000_000) + "M tok"
	}
}

func compactFloat(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
}

// humanDuration renders an elapsed duration compactly for the end-of-turn
// banner: `0.8s`, `12.3s`, `6m 51s`, `1h 14m`. Sub-minute keeps one decimal
// so quick turns stay informative; past a minute the decimal is dropped
// because sub-second precision is noise once you're counting minutes.
// Round values read as `1m` / `1h` rather than `1m 0s` / `1h 0m`.
func humanDuration(d time.Duration) string {
	s := d.Seconds()
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	return minutesOrHours(int(s))
}

// humanRate renders a tokens-per-second throughput for the end-of-turn
// banner: `25 tok/s`, `5.3 tok/s`, `0.5 tok/s`. Returns "" when the
// inputs are degenerate (no tokens or zero elapsed) so the caller can
// omit the segment cleanly. Sub-10 tok/s keeps one decimal because
// reasoning models often sit at 1.x tok/s where the decimal is the
// only meaningful signal; past 10 tok/s the decimal is noise. The unit
// mirrors the neighbouring `… tok` token counter so the status bar
// speaks one vocabulary.
func humanRate(tokens int, d time.Duration) string {
	if tokens <= 0 || d <= 0 {
		return ""
	}
	r := float64(tokens) / d.Seconds()
	if r >= 10 {
		return fmt.Sprintf("%d tok/s", int(r+0.5))
	}
	return fmt.Sprintf("%.1f tok/s", r)
}

// minutesOrHours formats integer seconds >= 60 as `6m 51s`, `1h 14m`, etc.
// Drops the trailing sub-unit when it is zero so round values render as
// `1m` / `1h` rather than `1m 0s` / `1h 0m`.
func minutesOrHours(s int) string {
	if s < 3600 {
		m, rem := s/60, s%60
		if rem == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, rem)
	}
	h, m := s/3600, (s%3600)/60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// backendLabel renders the "am I connected?" signal. Connected is the quiet
// default — the profile name, bold, no colour. Disconnected switches to bold
// yellow and appends a `!` marker so the state stays legible even on colour
// stripped terminals.
func backendLabel(c *config.Config, connected bool) string {
	if connected {
		return styleBackendOK.Render(c.Active)
	}
	return styleBackendWarn.Render(c.Active + " !")
}

// humanInt formats a non-negative integer with thin commas so a context
// window like 262144 reads as "262,144" rather than a wall of digits. Goal
// is scannability in the activation line, nothing more.
func humanInt(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + (len(s)-1)/3)
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	b.WriteString(s[:head])
	for i := head; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
