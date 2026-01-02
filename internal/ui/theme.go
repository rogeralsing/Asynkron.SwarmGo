package ui

import "github.com/charmbracelet/lipgloss"

type theme struct {
	header  lipgloss.Color
	accent  lipgloss.Color
	dim     lipgloss.Color
	border  lipgloss.Color
	focus   lipgloss.Color
	running lipgloss.Color
	error   lipgloss.Color
	say     lipgloss.Color
	do      lipgloss.Color
	see     lipgloss.Color
}

func defaultTheme() theme {
	return theme{
		header:  lipgloss.Color("#61afef"),
		accent:  lipgloss.Color("#e5c07b"),
		dim:     lipgloss.Color("#5c6370"),
		border:  lipgloss.Color("#5c6370"),
		focus:   lipgloss.Color("#61afef"),
		running: lipgloss.Color("#98c379"),
		error:   lipgloss.Color("#e06c75"),
		say:     lipgloss.Color("#abb2bf"),
		do:      lipgloss.Color("#5c6370"),
		see:     lipgloss.Color("#4b5263"),
	}
}
