package ui

import "github.com/nlm/duetty/internal/wordalign"

const (
	corrStyle     = "[blue]"    // corrected words in a final row
	liveCorrStyle = "[blue::i]" // corrected words in a live row
)

// correctionSegs returns the corrected text as segments: words that also
// appear (in order, ignoring case and punctuation) in the heard text get
// base, the others get mark. Adjacent words of the same style are merged.
func correctionSegs(heard, corrected, base, mark string) []seg {
	c := wordalign.Tokens(corrected)
	kept := make([]bool, len(c))
	for _, op := range wordalign.Align(wordalign.Tokens(heard), c) {
		if op.Kind == wordalign.Equal {
			kept[op.B0] = true
		}
	}
	var segs []seg
	var lastKept bool
	for j, t := range c {
		punct := wordalign.Norm(t.Text) == ""
		// punctuation-only words are never worth marking
		keep := kept[j] || punct
		style := mark
		if keep {
			style = base
		}
		if n := len(segs); n > 0 && (keep == lastKept || punct) {
			if t.Sp {
				segs[n-1].text += " "
			}
			segs[n-1].text += t.Text
		} else {
			segs = append(segs, seg{style, t.Text, len(segs) > 0 && !t.Sp})
			lastKept = keep
		}
	}
	return segs
}
