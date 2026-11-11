package top

import (
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// barW is the width of the bar in a bar cell ("███░░░ 2.7/8").
const barW = 6

// Col is a table column. Columns fit their content: flex columns (paths,
// commands) take the remaining width or shrink to Min; when the frame is
// still too narrow, columns with a Drop rank are left out, highest first.
type Col struct {
	Title string
	Right bool // right-aligned (numbers)
	Bar   bool // cells carry a bar before the value
	Flex  bool
	Min   int // minimum width of a flex column
	Drop  int // > 0: may be dropped, higher first
}

// Cell is one table cell; Frac (0..1) draws the bar of a Bar column.
type Cell struct {
	Text   string
	Frac   float64
	HasBar bool
}

// C is a plain cell.
func C(s string) Cell { return Cell{Text: s} }

// B is a bar cell of value/total with its text.
func B(value, total float64, text string) Cell {
	f := 0.0
	if total > 0 {
		f = math.Min(1, math.Max(0, value/total))
	}
	return Cell{Text: text, Frac: f, HasBar: true}
}

// layoutCols computes the column widths for width cells (gaps of one
// space); dropped columns get width -1.
func layoutCols(cols []Col, rows [][]Cell, width int) []int {
	ws := make([]int, len(cols))
	for i, c := range cols {
		w := ansi.StringWidth(c.Title) + 1 // room for the sort marker
		for _, r := range rows {
			if i < len(r) {
				cw := ansi.StringWidth(r[i].Text)
				if c.Bar {
					cw += barW + 1
				}
				w = max(w, cw)
			}
		}
		ws[i] = w
	}
	total := func() int {
		n, s := 0, 0
		for _, w := range ws {
			if w >= 0 {
				s += w
				n++
			}
		}
		return s + max(n-1, 0)
	}
	// Shrink flex columns, then drop columns by rank.
	for total() > width {
		shrunk := false
		for i, c := range cols {
			if c.Flex && ws[i] > max(c.Min, 4) && total() > width {
				ws[i]--
				shrunk = true
			}
		}
		if shrunk {
			continue
		}
		best := -1
		for i, c := range cols {
			if ws[i] >= 0 && c.Drop > 0 && (best < 0 || c.Drop > cols[best].Drop) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		ws[best] = -1
	}
	// Spread spare width over the flex columns.
	if extra := width - total(); extra > 0 {
		var flex []int
		for i, c := range cols {
			if c.Flex && ws[i] >= 0 {
				flex = append(flex, i)
			}
		}
		for k, i := range flex {
			share := extra / len(flex)
			if k == len(flex)-1 {
				share = extra - share*(len(flex)-1)
			}
			ws[i] += share
		}
	}
	return ws
}

// renderRow renders one row with the given widths.
func renderRow(cols []Col, ws []int, r []Cell, st Styles) string {
	var parts []string
	for i, c := range cols {
		w := ws[i]
		if w < 0 {
			continue
		}
		var cell Cell
		if i < len(r) {
			cell = r[i]
		}
		if c.Bar && cell.HasBar && w > barW+1 {
			n := int(math.Round(cell.Frac * barW))
			b := st.Bar.Render(strings.Repeat("█", n)) + st.BarBg.Render(strings.Repeat("░", barW-n))
			parts = append(parts, b+" "+padLeft(cell.Text, w-barW-1))
			continue
		}
		if c.Right || c.Bar {
			parts = append(parts, padLeft(cell.Text, w))
		} else {
			parts = append(parts, pad(cell.Text, w))
		}
	}
	return strings.Join(parts, " ")
}

// renderHeader renders the column titles; marks[i] is appended to the
// title of column i (sort direction).
func renderHeader(cols []Col, ws []int, marks map[int]string) string {
	r := make([]Cell, len(cols))
	for i, c := range cols {
		r[i] = C(c.Title + marks[i])
	}
	hc := make([]Col, len(cols))
	for i, c := range cols {
		hc[i] = Col{Title: c.Title, Right: c.Right && !c.Bar}
	}
	return renderRow(hc, ws, r, Styles{})
}

func padLeft(s string, w int) string {
	s = trunc(s, w)
	if n := w - ansi.StringWidth(s); n > 0 {
		s = strings.Repeat(" ", n) + s
	}
	return s
}
