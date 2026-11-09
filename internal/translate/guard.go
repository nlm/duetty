package translate

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nlm/duetty/internal/wordalign"
)

// minSpanSimilarity is the least similarity between a heard span and the
// span that replaces it; below it the two don't sound alike and the
// replacement is dropped.
const minSpanSimilarity = 0.5

// maxInsertWords is the most words a correction may add in one place.
const maxInsertWords = 2

// minContextWordRunes is the length from which a context word counts.
const minContextWordRunes = 3

// stopWords are common function words (en, fr, es, de, it, pt). They are
// never context words, so restoring a missing "the" is not mistaken for
// bringing in a context term.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		the and for with that this from are was were have has had not but you your
		our its they them their there what when who will can all any into about
		out one some more than then also just been would could should which
		les des une est que qui dans pour par sur avec pas mais ses son sont aux
		ces cette leur nous vous ils elle elles comme tout plus être avoir fait
		mon ton mes tes
		los las una uno del por para con sin sus más pero como este esta esto
		ese esa son está hay muy todo también nos les
		der die das den dem des und ein eine einen einem einer ist sind mit von
		für auf aus bei nicht auch sich wie oder aber wir ihr sie zum zur dass
		gli una uno del della dei delle che non per con sono come anche più alla
		nel nella questo questa suo sua
		uma uns umas dos das que não por para com sem seu sua são como mais mas
		nos nas este esta isso também`) {
		stopWords[w] = true
	}
}

// contextWords returns the normalised words of at least
// minContextWordRunes runes in the context body, minus stopWords.
func contextWords(body string) map[string]bool {
	words := map[string]bool{}
	for _, t := range wordalign.Tokens(body) {
		for _, n := range subWords(t.Text) {
			if utf8.RuneCountInString(n) >= minContextWordRunes && !stopWords[n] {
				words[n] = true
			}
		}
	}
	return words
}

// subWords returns the normalised token and its letter/digit runs
// ("Dan's" -> "dans", "dan", "s"), so a name inside a token is found.
func subWords(s string) []string {
	out := []string{wordalign.Norm(s)}
	parts := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(parts) > 1 {
		for _, p := range parts {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}

// checkCorrection aligns heard and corrected word by word and keeps only
// the changes that look like fixed mishearings: replacements that sound
// alike, small insertions that bring in neither a name nor an unheard
// context word, and deletions of a repeated word. It returns the text with
// the kept changes, and whether any change was dropped.
func checkCorrection(heard, corrected string, contextWords map[string]bool) (checked string, dropped bool) {
	h, c := wordalign.Tokens(heard), wordalign.Tokens(corrected)
	heardWords := map[string]bool{}
	for _, t := range h {
		for _, n := range subWords(t.Text) {
			heardWords[n] = true
		}
	}
	protected := func(j int) bool { return isProtected(c, j, heardWords, contextWords) }
	var out []wordalign.Token
	for _, op := range wordalign.Align(h, c) {
		var keep bool
		switch op.Kind {
		case wordalign.Equal:
			keep = true
		case wordalign.Replace:
			keep = replaceOK(h[op.A0:op.A1], c, op.B0, op.B1, protected)
		case wordalign.Insert:
			keep = insertOK(c, op.B0, op.B1, protected)
		case wordalign.Delete:
			keep = deleteOK(h, op.A0, op.A1)
		}
		if keep {
			out = append(out, c[op.B0:op.B1]...)
		} else {
			out = append(out, h[op.A0:op.A1]...)
			dropped = true
		}
	}
	return wordalign.Join(out), dropped
}

// isProtected reports whether corrected token c[j] needs a heard
// counterpart: it holds a context word not heard in the sentence, or it is
// capitalised and not the first token of the sentence.
func isProtected(c []wordalign.Token, j int, heardWords, contextWords map[string]bool) bool {
	if wordalign.Norm(c[j].Text) == "" {
		return false // punctuation only
	}
	for _, n := range subWords(c[j].Text) {
		if contextWords[n] && !heardWords[n] {
			return true
		}
	}
	return j > 0 && startsUpper(c[j].Text)
}

// replaceOK reports whether heard tokens h may be replaced by corrected
// tokens c[b0:b1]: the two spans sound alike (word by word when they have
// the same number of words, as a whole for splits and merges), and each
// protected corrected token sounds like some contiguous part of h.
func replaceOK(h, c []wordalign.Token, b0, b1 int, protected func(int) bool) bool {
	if n := b1 - b0; n == len(h) && n > 1 {
		for k := range n {
			if similarity(h[k].Text, c[b0+k].Text) < minSpanSimilarity {
				return false
			}
		}
	} else if similarity(wordalign.Join(h), wordalign.Join(c[b0:b1])) < minSpanSimilarity {
		return false
	}
	for j := b0; j < b1; j++ {
		if protected(j) && !soundsLikePart(c[j].Text, h) {
			return false
		}
	}
	return true
}

// soundsLikePart reports whether word sounds like some contiguous run of h.
func soundsLikePart(word string, h []wordalign.Token) bool {
	for i := range h {
		for k := i + 1; k <= len(h); k++ {
			if similarity(wordalign.Join(h[i:k]), word) >= minSpanSimilarity {
				return true
			}
		}
	}
	return false
}

// insertOK reports whether corrected tokens c[b0:b1], absent from the
// heard text, may be added: at most maxInsertWords words, none protected.
func insertOK(c []wordalign.Token, b0, b1 int, protected func(int) bool) bool {
	words := 0
	for j := b0; j < b1; j++ {
		if wordalign.Norm(c[j].Text) == "" {
			continue // punctuation only
		}
		words++
		if protected(j) {
			return false
		}
	}
	return words <= maxInsertWords
}

// deleteOK reports whether heard tokens h[a0:a1] may be dropped: each is
// punctuation only or repeats a heard neighbour ("the the" → "the").
func deleteOK(h []wordalign.Token, a0, a1 int) bool {
	for i := a0; i < a1; i++ {
		n := wordalign.Norm(h[i].Text)
		if n == "" {
			continue
		}
		if (i > 0 && wordalign.Norm(h[i-1].Text) == n) || (i+1 < len(h) && wordalign.Norm(h[i+1].Text) == n) {
			continue
		}
		return false
	}
	return true
}

// startsUpper reports whether the first letter of s is upper case.
func startsUpper(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return unicode.IsUpper(r)
		}
	}
	return false
}
