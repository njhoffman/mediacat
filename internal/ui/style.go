// Package ui holds terminal styling, formatting, tables and progress display.
package ui

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

var (
	colAccent = lipgloss.AdaptiveColor{Light: "#5A3FC0", Dark: "#A48CFF"}
	colOK     = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#56D364"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#E3B341"}
	colErr    = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#FF7B72"}
	colDim    = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
)

// Styles used across commands.
var (
	Title  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	Header = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	Dim    = lipgloss.NewStyle().Foreground(colDim)
	OK     = lipgloss.NewStyle().Foreground(colOK)
	Warn   = lipgloss.NewStyle().Foreground(colWarn)
	Err    = lipgloss.NewStyle().Foreground(colErr)
	Bold   = lipgloss.NewStyle().Bold(true)
	Accent = lipgloss.NewStyle().Foreground(colAccent)
	Plain  = lipgloss.NewStyle()
)

// DisableColor turns off all styling.
func DisableColor() { lipgloss.SetColorProfile(termenv.Ascii) }

// IsTerminal reports whether w is an interactive terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Width returns the terminal width of w, or 0 when it is not a terminal.
func Width(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}
