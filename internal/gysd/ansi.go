package gysd

import "regexp"

// ansiRE matches a CSI-style ANSI escape (ESC [ … final-byte) plus the
// shorter ESC ] OSC sequences pytest, npm, etc. emit. Storing color codes
// verbatim would make evidence-match brittle — the model reads stripped
// output in its tool-result, so it would quote the stripped form, but
// VerifyLog kept the colored form: substring miss every time. Strip on
// store fixes that asymmetry once.
var ansiRE = regexp.MustCompile("(?:\x1b\\[[0-?]*[ -/]*[@-~])|(?:\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\))")

func stripANSI(s string) string {
	if !needsStrip(s) {
		return s
	}
	return ansiRE.ReplaceAllString(s, "")
}

// needsStrip is a fast pre-check so the regex doesn't run on the common
// case (no escape codes at all).
func needsStrip(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return true
		}
	}
	return false
}
