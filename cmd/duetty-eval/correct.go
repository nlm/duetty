package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nlm/duetty/internal/translate"
)

// sameWords reports whether a and b hold the same words, ignoring case,
// punctuation and whitespace differences.
func sameWords(a, b string) bool { return normWords(a) == normWords(b) }

// normWords lowercases s, drops punctuation and symbols (so an added comma
// in a script without spaces does not split a word) and collapses spaces.
func normWords(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return unicode.ToLower(r)
		case unicode.IsSpace(r):
			return ' '
		}
		return -1
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// median returns the median of ds (0 when empty).
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if n := len(s); n%2 == 0 {
		return (s[n/2-1] + s[n/2]) / 2
	}
	return s[len(s)/2]
}

// correctStats accumulates the -correct measurements over all clips and pairs.
type correctStats struct {
	heardOK   int // clips whose heard text matches the reference
	rephrased int // of those, clips the correction changed
	discarded int // of those, clips whose correction came back empty (unusable or discarded)
	errors    int // clips where either translate call failed
	plain     []time.Duration
	correct   []time.Duration
}

// line translates heard from -> to twice (plain, then with correction),
// timing each call, and returns the report line for the clip.
func (s *correctStats) line(ctx context.Context, tr translate.Translator, id, heard, ref, from, to string) string {
	call := func(correct bool) (translate.Result, time.Duration, error) {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		t0 := time.Now()
		r, err := tr.Translate(cctx, translate.Request{Text: heard, From: from, To: to, Correct: correct})
		return r, time.Since(t0), err
	}
	_, dPlain, err := call(false)
	if err != nil {
		s.errors++
		return fmt.Sprintf("  %s: error: %v", id, err)
	}
	r, dCorrect, err := call(true)
	if err != nil {
		s.errors++
		return fmt.Sprintf("  %s: error: %v", id, err)
	}
	s.plain = append(s.plain, dPlain)
	s.correct = append(s.correct, dCorrect)
	corrected, tag := r.Corrected, ""
	ok := sameWords(heard, ref)
	switch {
	case corrected == "":
		// Unusable reply, fallback or too dissimilar: not "unchanged".
		corrected = heard
		if ok {
			s.heardOK++
			s.discarded++
			tag = "  DISCARDED"
		}
	case ok:
		s.heardOK++
		if !sameWords(heard, corrected) {
			s.rephrased++
			tag = "  REPHRASE"
		}
	}
	return fmt.Sprintf("  %s: heard=%q corrected=%q%s", id, heard, corrected, tag)
}

// summary renders the closing report lines.
func (s *correctStats) summary() string {
	return fmt.Sprintf("rephrased %d/%d (clips heard correctly)\ndiscarded %d/%d\nerrors %d\nlatency median plain %d ms, correct %d ms",
		s.rephrased, s.heardOK, s.discarded, s.heardOK, s.errors, median(s.plain).Milliseconds(), median(s.correct).Milliseconds())
}
