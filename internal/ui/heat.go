package ui

import (
	"math"

	"github.com/charmbracelet/lipgloss"
	colorful "github.com/lucasb-eyer/go-colorful"
)

// Heat gradients from "just over the threshold" to "at or past the maximum": a pale
// yellow tint, through amber, to red. Light terminals get darker shades for contrast.
var (
	heatDark  = []string{"#EDE3A1", "#F2C14E", "#F28C38", "#FF4D4D"}
	heatLight = []string{"#9C8A00", "#B87A00", "#C25700", "#CF222E"}
)

const heatSteps = 10

var heatStyles = buildHeat()

func blend(stops []string, t float64) string {
	seg := t * float64(len(stops)-1)
	i := min(int(seg), len(stops)-2)
	a, _ := colorful.Hex(stops[i])
	b, _ := colorful.Hex(stops[i+1])
	return a.BlendLab(b, seg-float64(i)).Clamped().Hex()
}

func buildHeat() []lipgloss.Style {
	out := make([]lipgloss.Style, heatSteps)
	for i := range heatSteps {
		t := float64(i) / float64(heatSteps-1)
		st := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: blend(heatLight, t), Dark: blend(heatDark, t)})
		out[i] = st.Bold(t >= 0.5)
	}
	return out
}

// Heat returns the style for a value whose level past its threshold is level (0..1), or
// nil when level is 0 (within the threshold) so the cell keeps its normal style.
func Heat(level float64) *lipgloss.Style {
	if level <= 0 || math.IsNaN(level) {
		return nil
	}
	i := int(math.Ceil(min(level, 1) * float64(heatSteps-1)))
	return &heatStyles[i]
}
