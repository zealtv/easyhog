package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/zealtv/easyhog/internal/core"
)

// styles follow EasyLoom's palette: the terminal's own hues for state, one
// cyan brand colour, and two greys that stay readable on dark and light
// backgrounds. lipgloss drops colour for NO_COLOR and redirected output.
type styles struct {
	brand, faint, soft, warn, selected, hint, hintKey, match lipgloss.Style
	bar, barDim, thumb                                       lipgloss.Style
	border                                                   lipgloss.Style
	state                                                    map[State]lipgloss.Style
	// beat colours the heartbeat mark after a state word, never the word itself.
	beat map[core.Heartbeat]lipgloss.Style

	// The preview's own structure: a section heading, and a field's label.
	section, label lipgloss.Style

	// A payload file's contents, kept apart from the chrome above because these
	// style what a file says rather than the program's furniture. The markdown
	// ones follow EasyLoom's; the JSON ones tell a key from a string.
	mdHeading, mdStrong, mdEmphasis, mdInlineCode, mdCode lipgloss.Style
	mdBullet, mdQuote, mdLink, mdRule                     lipgloss.Style
	jsonKey, jsonString                                   lipgloss.Style
}

// colourOn says whether lipgloss will emit colour at all: it does not for
// NO_COLOR, a dumb terminal or redirected output. The preview keeps a file's
// markers exactly as written when it does not, so it asks once, up front.
func colourOn() bool {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render("x") != "x"
}

func newStyles() styles {
	faint := lipgloss.AdaptiveColor{Light: "243", Dark: "244"}
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	return styles{
		brand:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		faint:    lipgloss.NewStyle().Foreground(faint),
		soft:     lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "248"}),
		warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		selected: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81")),
		hint:     dim,
		hintKey:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")),
		match:    lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Underline(true),
		bar:      lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "254", Dark: "237"}),
		barDim:   lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "255", Dark: "235"}),
		thumb:    lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		border:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")),

		section: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "248"}),
		label:   lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "248"}),

		mdHeading:    lipgloss.NewStyle().Bold(true),
		mdStrong:     lipgloss.NewStyle().Bold(true),
		mdEmphasis:   lipgloss.NewStyle().Italic(true),
		mdInlineCode: lipgloss.NewStyle().Foreground(lipgloss.Color("151")),
		mdCode:       dim,
		mdBullet:     dim,
		mdQuote:      dim,
		mdLink:       lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		mdRule:       dim,
		jsonKey:      lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		jsonString:   lipgloss.NewStyle().Foreground(lipgloss.Color("151")),
		beat: map[core.Heartbeat]lipgloss.Style{
			core.HeartbeatFresh: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
			core.HeartbeatLate:  lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
			core.HeartbeatQuiet: lipgloss.NewStyle().Foreground(faint),
		},
		state: map[State]lipgloss.Style{
			StateStopped:     lipgloss.NewStyle().Foreground(faint),
			StateRunning:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
			StatePaused:      lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
			StateExternal:    lipgloss.NewStyle().Foreground(lipgloss.Color("141")),
			StateUnsupported: lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
			StatePending:     lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
			StateFailed:      lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
			StateScheduled:   lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "248"}),
		},
	}
}
