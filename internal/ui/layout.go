// Package ui is poly's two-pane terminal interface.
package ui

import (
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/nlm/poly/internal/convo"
	"github.com/rivo/tview"
)

const (
	srcStyle  = "[white::b]"
	softStyle = "[gray]"
	dimStyle  = "[gray::d]"
	pendStyle = "[gray::i]"
	errStyle  = "[red]"
	reset     = "[-::-]"
)

// ColumnWidth is the width in cells of each pane for a total screen width:
// one column is reserved for the separator.
func ColumnWidth(width int) int {
	w := (width - 1) / 2
	if w < 1 {
		w = 1
	}
	return w
}

// Layout renders rows into two equal-height columns of tview-tagged lines.
// width is the total screen width; each column is ColumnWidth(width) cells
// wide, measured in display cells (CJK is double width). Every row occupies
// the same line range in both columns, followed by one blank line.
func Layout(rows []convo.Row, pair [2]string, width int, timestamps bool) (left, right []string) {
	cw := ColumnWidth(width)
	for i, r := range rows {
		if i > 0 {
			left = append(left, "")
			right = append(right, "")
		}
		src, oth := renderRow(r, cw, timestamps)
		srcIsLeft := r.Lang != pair[1]
		a, b := src, oth
		if !srcIsLeft {
			a, b = oth, src
		}
		for len(a) < len(b) {
			a = append(a, "")
		}
		for len(b) < len(a) {
			b = append(b, "")
		}
		left = append(left, a...)
		right = append(right, b...)
	}
	return left, right
}

// renderRow returns the source-side and other-side lines of one row.
func renderRow(r convo.Row, cw int, ts bool) (src, oth []string) {
	stamp := ""
	if ts && !r.At.IsZero() {
		stamp = r.At.Format("15:04") + " "
	}
	if stamp != "" && cw < len(stamp)+2+2 {
		stamp = "" // no room for the timestamp plus one wide rune
	}
	indent := runewidth.StringWidth(stamp) + 2
	pad := strings.Repeat(" ", indent)
	first := func(marker string) string { return stamp + marker }

	cancelled := r.Status == convo.Cancelled
	origStyle, othStyle := srcStyle, softStyle
	if cancelled {
		origStyle = dimStyle
	}

	block := func(style, firstPrefix, text string) []string {
		lines := wrap(text, cw-indent)
		out := make([]string, 0, len(lines))
		for i, ln := range lines {
			p := pad
			if i == 0 {
				p = firstPrefix
			}
			out = append(out, p+style+tview.Escape(ln)+reset)
		}
		return out
	}

	switch {
	case r.Status == convo.Failed && strings.TrimSpace(r.Original) == "":
		// Transcription failed: nothing to show but the error.
		return block(errStyle, first("  "), r.Err), nil
	}
	src = block(origStyle, first("● "), r.Original)
	switch r.Status {
	case convo.Original:
		oth = []string{first("  ") + pendStyle + "…translating" + reset}
	case convo.Cancelled:
		oth = []string{first("  ") + dimStyle + "cancelled" + reset}
	case convo.Failed:
		oth = block(errStyle, first("  "), r.Err)
	default:
		oth = block(othStyle, first("  "), r.Translation)
	}
	return src, oth
}

// wrap breaks text into lines of at most width display cells: CJK (any wide
// rune) may break anywhere, other text breaks at spaces, and over-long words
// are split by rune. A single rune wider than width gets its own line.
func wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		if curW > 0 {
			lines = append(lines, cur.String())
		}
		cur.Reset()
		curW = 0
	}
	put := func(s string, w int) {
		cur.WriteString(s)
		curW += w
	}
	rs := []rune(text)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			// collapse whitespace; drop at line start
			for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t' || rs[i] == '\n' || rs[i] == '\r') {
				i++
			}
			if curW > 0 && curW+1 <= width && i < len(rs) {
				// only keep the space if the next token fits too (checked below)
				put(" ", 1)
			}
		case runewidth.RuneWidth(c) > 1:
			w := runewidth.RuneWidth(c)
			if curW+w > width {
				flush()
			}
			put(string(c), w)
			i++
		default:
			j := i
			ww := 0
			for j < len(rs) && rs[j] != ' ' && rs[j] != '\t' && rs[j] != '\n' && rs[j] != '\r' && runewidth.RuneWidth(rs[j]) <= 1 {
				ww += runewidth.RuneWidth(rs[j])
				j++
			}
			word := string(rs[i:j])
			if curW > 0 && curW+ww > width {
				trimTrailingSpace(&cur, &curW)
				flush()
			}
			if ww > width { // hard split
				for _, r := range word {
					rw := runewidth.RuneWidth(r)
					if curW+rw > width {
						flush()
					}
					put(string(r), rw)
				}
			} else {
				put(word, ww)
			}
			i = j
		}
	}
	trimTrailingSpace(&cur, &curW)
	flush()
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

func trimTrailingSpace(b *strings.Builder, w *int) {
	s := b.String()
	if t := strings.TrimRight(s, " "); t != s {
		*w -= len(s) - len(t)
		b.Reset()
		b.WriteString(t)
	}
}
