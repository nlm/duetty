package ui

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
	"github.com/nlm/poly/internal/convo"
)

// TestWriteREADMEScreenshot renders a sample session on a simulated
// terminal and converts it to an SVG. The SVG is always built and checked;
// it is written to docs/poly-screen.svg (the README picture) only with
// POLY_WRITE_SVG=1:
//
//	POLY_WRITE_SVG=1 go test ./internal/ui -run TestWriteREADMEScreenshot
func TestWriteREADMEScreenshot(t *testing.T) {
	a, sim := newSim(t, 96, 18)
	rows := []convo.Row{
		row(1, convo.Done, "fr", "Bonjour, vous avez une table pour deux ?", "Hello, do you have a table for two?"),
		row(2, convo.Done, "en", "Yes, inside or on the terrace?", "Oui, à l'intérieur ou en terrasse ?"),
		row(3, convo.Done, "fr", "En terrasse, s'il vous plaît.", "On the terrace, please."),
		{Seq: 4, Status: convo.Live, Lang: "fr", Original: "Et est-ce qu'on peut", Translation: "And can we", Stale: true},
	}
	a.SetRows(rows)
	a.SetStatus(0.45, "listening", 0, false)
	a.SetUsage("≈ €0.004 · 2.1k tok · 1m12s audio")
	waitFor(t, "sample drawn", func() bool {
		settle(a)
		o := dump(sim)
		return strings.Contains(o, "And can we") && strings.Contains(o, "1m12s audio")
	})
	svg := screenSVG(sim)
	if err := checkXML(svg); err != nil {
		t.Fatalf("invalid SVG: %v", err)
	}
	if len(svg) > 40<<10 {
		t.Fatalf("SVG is %d bytes, want < 40 KB", len(svg))
	}
	if os.Getenv("POLY_WRITE_SVG") != "1" {
		t.Skip("set POLY_WRITE_SVG=1 to write docs/poly-screen.svg")
	}
	if err := os.WriteFile("../../docs/poly-screen.svg", []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkXML(s string) error {
	d := xml.NewDecoder(strings.NewReader(s))
	for {
		if _, err := d.Token(); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// Terminal look of the picture.
const (
	svgBG      = "#1e1e1e"
	svgFG      = "#d4d4d4"
	svgCellW   = 8.4 // px per column at svgFont
	svgCellH   = 18  // px per row
	svgFont    = 14  // px
	svgPad     = 12  // px around the grid
	svgBaseOff = 13  // baseline offset within a row
)

var svgColors = map[tcell.Color]string{
	tcell.ColorGray:   "#8a8a8a",
	tcell.ColorGreen:  "#6abf69",
	tcell.ColorYellow: "#d7ba7d",
	tcell.ColorRed:    "#f14c4c",
}

func svgColor(c tcell.Color, def string) string {
	if c == tcell.ColorDefault || c == tcell.ColorReset {
		return def
	}
	if h, ok := svgColors[c]; ok {
		return h
	}
	return fmt.Sprintf("#%06x", c.Hex())
}

// screenSVG draws the screen's cells as text runs: one <text> per run of
// cells with the same style; runs of plain spaces are left out.
func screenSVG(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	width := float64(w)*svgCellW + 2*svgPad
	height := float64(h*svgCellH + 2*svgPad)
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`+"\n", width, height, width, height)
	fmt.Fprintf(&sb, `<rect width="100%%" height="100%%" rx="8" fill="%s"/>`+"\n", svgBG)
	fmt.Fprintf(&sb, `<g font-family="ui-monospace, Menlo, Consolas, 'DejaVu Sans Mono', monospace" font-size="%d" fill="%s" xml:space="preserve">`+"\n", svgFont, svgFG)
	for y := 0; y < h; y++ {
		x := 0
		for x < w {
			start, style := x, cells[y*w+x].Style
			var text strings.Builder
			cols := 0
			for x < w && cells[y*w+x].Style == style {
				r := ' '
				if c := cells[y*w+x]; len(c.Runes) > 0 && c.Runes[0] != 0 {
					r = c.Runes[0]
				}
				text.WriteRune(r)
				cw := max(runewidth.RuneWidth(r), 1)
				x += cw
				cols += cw
			}
			writeRun(&sb, start, y, cols, text.String(), style)
		}
	}
	sb.WriteString("</g>\n</svg>\n")
	return sb.String()
}

func writeRun(sb *strings.Builder, col, row, cols int, text string, style tcell.Style) {
	fg, bg, attr := style.Decompose()
	x := svgPad + float64(col)*svgCellW
	top := float64(svgPad + row*svgCellH)
	if bg != tcell.ColorDefault && bg != tcell.ColorReset {
		fmt.Fprintf(sb, `<rect x="%.1f" y="%.0f" width="%.1f" height="%d" fill="%s"/>`+"\n",
			x, top, float64(cols)*svgCellW, svgCellH, svgColor(bg, svgBG))
	}
	trimmed := strings.TrimRight(text, " ")
	if trimmed == "" && attr&(tcell.AttrUnderline|tcell.AttrStrikeThrough) == 0 {
		return
	}
	if trimmed != "" {
		text, cols = trimmed, cols-(len([]rune(text))-len([]rune(trimmed)))
	}
	lead := len(text) - len(strings.TrimLeft(text, " "))
	text, x, cols = text[lead:], x+float64(lead)*svgCellW, cols-lead
	var attrs strings.Builder
	if f := svgColor(fg, svgFG); f != svgFG {
		fmt.Fprintf(&attrs, ` fill="%s"`, f)
	}
	if attr&tcell.AttrItalic != 0 {
		attrs.WriteString(` font-style="italic"`)
	}
	if attr&tcell.AttrBold != 0 {
		attrs.WriteString(` font-weight="bold"`)
	}
	if attr&tcell.AttrDim != 0 {
		attrs.WriteString(` opacity="0.6"`)
	}
	var deco []string
	if attr&tcell.AttrUnderline != 0 {
		deco = append(deco, "underline")
	}
	if attr&tcell.AttrStrikeThrough != 0 {
		deco = append(deco, "line-through")
	}
	if len(deco) > 0 {
		fmt.Fprintf(&attrs, ` text-decoration="%s"`, strings.Join(deco, " "))
	}
	// textLength keeps every run on the cell grid whatever the font's advance.
	fmt.Fprintf(sb, `<text x="%.1f" y="%.0f" textLength="%.1f" lengthAdjust="spacingAndGlyphs"%s>%s</text>`+"\n",
		x, top+svgBaseOff, float64(cols)*svgCellW, attrs.String(), escapeXML(text))
}

func escapeXML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
