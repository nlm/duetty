// Package ui is duetty's two-pane terminal interface.
package ui

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	"github.com/nlm/duetty/internal/convo"
	"github.com/rivo/tview"
)

const (
	origStyle  = "[gray]"    // what was heard or typed: input check only
	transStyle = "[-::-]"    // the translation: terminal default color, no attributes
	cancStyle  = "[gray::s]" // cancelled original
	dimStyle   = "[gray::d]" // cancelled marker
	pendStyle  = "[gray::i]" // translation in flight
	liveStyle  = "[gray::i]" // provisional live text and its draft translation
	errStyle   = "[red]"
	reset      = "[-::-]"
	gutter     = " " // blank cell on each side of a pane's text
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

// seg is a styled run of text inside a paragraph.
type seg struct{ style, text string }

// group is a run of consecutive rows spoken in the same language: shown as
// one paragraph per side instead of one paragraph per sentence.
type group struct {
	lang     string
	at       time.Time
	src, oth []seg
	failed   string // transcription failure (a group of its own)
}

// groups merges consecutive same-language rows.
func groups(rows []convo.Row) []group {
	var gs []group
	for _, r := range rows {
		if r.Status == convo.Failed && strings.TrimSpace(r.Original) == "" {
			gs = append(gs, group{at: r.At, failed: r.Err})
			continue
		}
		if n := len(gs); n == 0 || gs[n-1].failed != "" || gs[n-1].lang != r.Lang {
			gs = append(gs, group{lang: r.Lang, at: r.At})
		}
		g := &gs[len(gs)-1]
		os := origStyle
		if r.Status == convo.Cancelled {
			os = cancStyle
		}
		if r.Status == convo.Live {
			g.src = append(g.src, seg{liveStyle, r.Original})
			if r.Translation == "" {
				g.oth = append(g.oth, seg{liveStyle, "…"})
				continue
			}
			g.oth = append(g.oth, seg{liveStyle, r.Translation})
			if r.Stale {
				g.oth = append(g.oth, seg{liveStyle, "…"})
			}
			continue
		}
		g.src = append(g.src, seg{os, r.Original})
		switch r.Status {
		case convo.Original:
			g.oth = append(g.oth, seg{pendStyle, "…"})
		case convo.Cancelled:
			g.oth = append(g.oth, seg{dimStyle, "(cancelled)"})
		case convo.Failed:
			g.oth = append(g.oth, seg{errStyle, "(" + r.Err + ")"})
		default:
			g.oth = append(g.oth, seg{transStyle, r.Translation})
		}
	}
	return gs
}

// Layout renders rows into two equal-height columns of tview-tagged lines.
// width is the total screen width; each column is ColumnWidth(width) cells
// wide, measured in display cells (CJK is double width), with a one-cell
// gutter on each side. Consecutive rows in the same language form one
// paragraph; each paragraph occupies the same line range in both columns,
// and paragraphs are separated by one blank line.
func Layout(rows []convo.Row, pair [2]string, width int, timestamps bool) (left, right []string) {
	cw, gut := ColumnWidth(width), gutter
	if cw < 8 {
		gut = "" // too narrow to spare the gutter
	}
	cw -= 2 * len(gut)
	for i, g := range groups(rows) {
		if i > 0 {
			left = append(left, "")
			right = append(right, "")
		}
		src, oth := renderGroup(g, cw, timestamps)
		a, b := src, oth
		if g.failed == "" && g.lang == pair[1] {
			a, b = oth, src
		}
		for len(a) < len(b) {
			a = append(a, "")
		}
		for len(b) < len(a) {
			b = append(b, "")
		}
		for _, ln := range a {
			left = append(left, gut+ln)
		}
		for _, ln := range b {
			right = append(right, gut+ln)
		}
	}
	return left, right
}

// renderGroup returns the source-side and other-side lines of one group.
func renderGroup(g group, cw int, ts bool) (src, oth []string) {
	stamp := ""
	if ts && !g.at.IsZero() {
		stamp = g.at.Format("15:04") + " "
	}
	if stamp != "" && cw < len(stamp)+2+2 {
		stamp = "" // no room for the timestamp plus one wide rune
	}
	indent := runewidth.StringWidth(stamp) + 2
	pad := strings.Repeat(" ", indent)
	block := func(marker string, segs []seg) []string {
		lines := wrapSegs(segs, cw-indent)
		for i := range lines {
			p := pad
			if i == 0 {
				p = stamp + marker
			}
			lines[i] = p + lines[i]
		}
		return lines
	}
	if g.failed != "" {
		return block("  ", []seg{{errStyle, g.failed}}), nil
	}
	return block(origStyle+"»"+reset+" ", g.src), block("  ", g.oth)
}

// joinable reports whether two texts join without a space (CJK to CJK).
func joinable(prev, next string) bool {
	p, _ := utf8.DecodeLastRuneInString(prev)
	n, _ := utf8.DecodeRuneInString(next)
	return runewidth.RuneWidth(p) > 1 && runewidth.RuneWidth(n) > 1
}

// wrapSegs wraps styled segments as one paragraph and returns tagged lines.
// wrap keeps every non-space rune in order, so each output rune is mapped
// back to its segment by counting non-space runes.
func wrapSegs(segs []seg, width int) []string {
	var plainText strings.Builder
	var owner []int // segment index of each non-space rune
	for i, s := range segs {
		t := strings.Join(strings.Fields(s.text), " ")
		if t == "" {
			continue
		}
		if plainText.Len() > 0 && !joinable(plainText.String(), t) {
			plainText.WriteByte(' ')
		}
		plainText.WriteString(t)
		for _, c := range t {
			if !unicode.IsSpace(c) {
				owner = append(owner, i)
			}
		}
	}
	lines := wrap(plainText.String(), width)
	k := 0
	out := make([]string, len(lines))
	for li, ln := range lines {
		var b strings.Builder
		cur := -1
		var run strings.Builder
		flush := func() {
			if run.Len() > 0 {
				b.WriteString(segs[cur].style + tview.Escape(run.String()) + reset)
				run.Reset()
			}
		}
		for _, c := range ln {
			o := cur
			if !unicode.IsSpace(c) && k < len(owner) {
				o = owner[k]
				k++
			}
			if o != cur {
				flush()
				cur = o
			}
			if cur < 0 { // leading space before any rune: cannot happen, wrap trims
				continue
			}
			run.WriteRune(c)
		}
		flush()
		out[li] = b.String()
	}
	return out
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
