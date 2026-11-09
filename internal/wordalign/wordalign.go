// Package wordalign splits sentences into word tokens and aligns two
// sentences word by word, ignoring case and punctuation.
package wordalign

import (
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
)

// Token is a word of a sentence; Sp reports whether a space precedes it.
type Token struct {
	Text string
	Sp   bool
}

// Tokens splits text on spaces; inside a word, each wide rune (CJK) is a
// token of its own and runs of other runes stay together.
func Tokens(text string) []Token {
	var out []Token
	for _, w := range strings.Fields(text) {
		sp := true
		var run []rune
		flush := func() {
			if len(run) > 0 {
				out = append(out, Token{string(run), sp})
				sp = false
				run = run[:0]
			}
		}
		for _, r := range w {
			if runewidth.RuneWidth(r) > 1 {
				flush()
				out = append(out, Token{string(r), sp})
				sp = false
				continue
			}
			run = append(run, r)
		}
		flush()
	}
	return out
}

// Join renders tokens back to text: a space before each token that had one,
// except the first.
func Join(toks []Token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && t.Sp {
			b.WriteByte(' ')
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// Norm case-folds a token and strips punctuation.
func Norm(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// Kind is the kind of an aligned op.
type Kind int

// Op kinds.
const (
	Equal   Kind = iota // same word in both
	Replace             // heard words replaced by other words
	Insert              // words only in b
	Delete              // words only in a
)

// Op is a span of a ([A0, A1)) aligned with a span of b ([B0, B1)).
type Op struct {
	Kind   Kind
	A0, A1 int
	B0, B1 int
}

// Align aligns token lists a and b by a longest common subsequence over
// normalised tokens (punctuation-only tokens never match) and returns the
// ops covering both lists in order. An Equal op spans one token.
func Align(a, b []Token) []Op {
	an, bn := make([]string, len(a)), make([]string, len(b))
	for i, t := range a {
		an[i] = Norm(t.Text)
	}
	for j, t := range b {
		bn[j] = Norm(t.Text)
	}
	// lcs[i][j]: longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case an[i] != "" && an[i] == bn[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []Op
	gap := func(i0, i, j0, j int) {
		switch {
		case i > i0 && j > j0:
			ops = append(ops, Op{Replace, i0, i, j0, j})
		case i > i0:
			ops = append(ops, Op{Delete, i0, i, j0, j})
		case j > j0:
			ops = append(ops, Op{Insert, i0, i, j0, j})
		}
	}
	i0, j0 := 0, 0
	for i, j := 0, 0; ; {
		if i == len(a) || j == len(b) {
			gap(i0, len(a), j0, len(b))
			return ops
		}
		switch {
		case an[i] != "" && an[i] == bn[j]:
			gap(i0, i, j0, j)
			ops = append(ops, Op{Equal, i, i + 1, j, j + 1})
			i++
			j++
			i0, j0 = i, j
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
}
