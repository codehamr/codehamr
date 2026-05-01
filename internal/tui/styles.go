package tui

import "github.com/charmbracelet/lipgloss"

// hamrColor is the single accent — "hot iron under the hammer". Every
// deliberate highlight in the UI uses this one colour; everything else is
// default terminal, dim, or a semantic warn/error colour. Consistency in
// visual language is how hamr stays legible at a glance.
var hamrColor = lipgloss.Color("208")

var (
	// Accent — used wherever a "here is an action / this is yours" signal
	// belongs: the textarea ▌ marker, successful command confirmations,
	// the spinner that runs during a turn, the selected popover row.
	styleHamr = lipgloss.NewStyle().Foreground(hamrColor)

	// Structural neutrals. styleDim is the workhorse for secondary copy
	// (banner, status bar, per-turn summary, popover descriptions).
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleStatus = lipgloss.NewStyle().Faint(true)

	// Semantic warn/error — the two places we deliberately break out of the
	// single-accent rule. Yellow warns, red errors. These are terminal
	// conventions; fighting them costs more than it gains.
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleError = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

	// Confirmations use the accent — distinct from plain text, visibly
	// "something good happened", but never loud.
	styleOK = lipgloss.NewStyle().Foreground(hamrColor)

	// Textarea prompt marker and the spinner share the accent: both mark
	// live activity.
	stylePrompt  = lipgloss.NewStyle().Foreground(hamrColor)
	styleSpinner = lipgloss.NewStyle().Foreground(hamrColor)

	// The user's echoed line in scrollback: bold default — distinct from
	// assistant markdown output without painting the user's text orange.
	styleUser = lipgloss.NewStyle().Bold(true)

	// Backend label: connected is the quiet default (bold, no colour);
	// disconnected SHOUTS with yellow + a `!` marker so the state is
	// obvious even on colour-stripped terminals.
	styleBackendOK   = lipgloss.NewStyle().Bold(true)
	styleBackendWarn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))

	// Popover: no backgrounds, no marker column. Selection is bold + accent
	// orange on the whole row; the "current" entry (e.g. active profile) is
	// bold without colour. Clean, aligned, no visual jut.
	stylePopoverRow      = lipgloss.NewStyle()
	stylePopoverCurrent  = lipgloss.NewStyle().Bold(true)
	stylePopoverSelected = lipgloss.NewStyle().Bold(true).Foreground(hamrColor)
)
