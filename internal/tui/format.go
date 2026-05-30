package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codehamr/codehamr/internal/config"
)

// humanTokens renders a token count compactly: `900 tok`, `1.2k tok`,
// `1.5M tok`. Trailing `.0` is trimmed so round multiples read as `1k`.
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

// humanDuration renders an elapsed duration compactly: `0.8s`, `6m 51s`,
// `1h 14m`. Sub-minute keeps one decimal; past a minute it's dropped as
// noise. Round values read as `1m` / `1h`, not `1m 0s` / `1h 0m`.
func humanDuration(d time.Duration) string {
	secs := d.Seconds()
	if secs < 60 {
		return fmt.Sprintf("%.1fs", secs)
	}
	s := int(secs)
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

// humanRate renders throughput: `25 tok/s`, `5.3 tok/s`. Returns "" on
// degenerate input (no tokens or zero elapsed) so the caller omits the
// segment. Sub-10 tok/s keeps one decimal — reasoning models sit at 1.x
// where that decimal is the only signal; above 10 it's noise.
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

// backendLabel renders the connection signal. Connected: profile name, bold,
// no colour. Disconnected: bold yellow plus a `!` marker, so the state stays
// legible on colour-stripped terminals.
func backendLabel(c *config.Config, connected bool) string {
	if connected {
		return styleBackendOK.Render(c.Active)
	}
	return styleBackendWarn.Render(c.Active + " !")
}

// humanInt formats a non-negative integer with commas so a context window
// like 262144 reads as "262,144" rather than a wall of digits.
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
