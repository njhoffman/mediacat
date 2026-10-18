package ui

import (
	"math"
	"sync"

	"github.com/charmbracelet/lipgloss"
	colorful "github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
)

// Heat gradients from "just over the threshold" to "at or past the maximum": a pale
// yellow tint, through amber, to red. Light terminals get darker shades for contrast.
var (
	heatDark  = []string{"#EDE3A1", "#F2C14E", "#F28C38", "#FF4D4D"}
	heatLight = []string{"#9C8A00", "#B87A00", "#C25700", "#CF222E"}
)

// Steps in the gradient: fine on 24-bit color terminals, coarse where the palette could
// not show the difference anyway.
const (
	heatStepsTrueColor = 64
	heatStepsLimited   = 10
)

var (
	heatOnce   sync.Once
	heatStyles []lipgloss.Style
)

func blend(stops []string, t float64) string {
	seg := t * float64(len(stops)-1)
	i := min(int(seg), len(stops)-2)
	a, _ := colorful.Hex(stops[i])
	b, _ := colorful.Hex(stops[i+1])
	return a.BlendLab(b, seg-float64(i)).Clamped().Hex()
}

func buildHeat(steps int) []lipgloss.Style {
	out := make([]lipgloss.Style, steps)
	for i := range steps {
		t := float64(i) / float64(steps-1)
		out[i] = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: blend(heatLight, t), Dark: blend(heatDark, t)})
	}
	return out
}

func heatSteps() int {
	if lipgloss.ColorProfile() == termenv.TrueColor {
		return heatStepsTrueColor
	}
	return heatStepsLimited
}

// Heat returns the style for a value whose level past its threshold is level (0..1), or
// nil when level is 0 (within the threshold) so the cell keeps its normal style. Color
// alone carries the level; it is never bold.
func Heat(level float64) *lipgloss.Style {
	if level <= 0 || math.IsNaN(level) {
		return nil
	}
	heatOnce.Do(func() { heatStyles = buildHeat(heatSteps()) })
	i := int(math.Ceil(min(level, 1) * float64(len(heatStyles)-1)))
	return &heatStyles[i]
}

// resetHeat discards cached styles (tests change the color profile).
func resetHeat() { heatOnce = sync.Once{} }
