package ui

import (
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Column describes one table column.
type Column struct {
	Title string
	Right bool // right-align (numbers)
	Flex  bool // absorbs width changes; at most one column should set it
	Style lipgloss.Style
}

// Cell overrides a column's style for one value.
type Cell struct {
	Text  string
	Style *lipgloss.Style
}

// Table renders rows as aligned columns. When width > 0 the Flex column is truncated in
// the middle to fit, keeping both ends of paths visible.
func Table(w io.Writer, cols []Column, rows [][]Cell, width int) {
	const gap = 2
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipgloss.Width(c.Title)
	}
	for _, r := range rows {
		for i := range cols {
			if i < len(r) {
				widths[i] = max(widths[i], lipgloss.Width(r[i].Text))
			}
		}
	}
	if width > 0 {
		total := gap * (len(cols) - 1)
		for _, n := range widths {
			total += n
		}
		for i, c := range cols {
			if c.Flex && total > width {
				widths[i] = max(widths[i]-(total-width), 12)
			}
		}
	}
	line := func(cells []Cell, header bool) {
		var b strings.Builder
		for i, c := range cols {
			text := ""
			var st lipgloss.Style
			if header {
				text, st = c.Title, Header
			} else if i < len(cells) {
				text, st = cells[i].Text, c.Style
				if cells[i].Style != nil {
					st = *cells[i].Style
				}
			}
			text = TruncateMiddle(text, widths[i])
			pad := strings.Repeat(" ", widths[i]-lipgloss.Width(text))
			last := i == len(cols)-1
			if c.Right {
				b.WriteString(pad + st.Render(text))
			} else if last {
				b.WriteString(st.Render(text))
			} else {
				b.WriteString(st.Render(text) + pad)
			}
			if !last {
				b.WriteString(strings.Repeat(" ", gap))
			}
		}
		io.WriteString(w, strings.TrimRight(b.String(), " ")+"\n")
	}
	line(nil, true)
	for _, r := range rows {
		line(r, false)
	}
}

// TruncateMiddle shortens s to n display cells by replacing its middle with an ellipsis.
func TruncateMiddle(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	if n <= 1 {
		return ansi.Truncate(s, n, "")
	}
	keep := n - 1
	head := keep / 2
	tail := keep - head
	return ansi.Truncate(s, head, "") + "…" + ansi.TruncateLeft(s, lipgloss.Width(s)-tail, "")
}

// Cells wraps strings as unstyled cells.
func Cells(vals ...string) []Cell {
	out := make([]Cell, len(vals))
	for i, v := range vals {
		out[i] = Cell{Text: v}
	}
	return out
}
