package stt

import (
	"context"
	"errors"
	"strings"
	"unicode"
)

// ErrNoise means the audio produced no usable speech.
var ErrNoise = errors.New("noise")

// Text-check length rule: lingua confidently misclassifies very short text,
// so the check is only applied to text with at least MinCheckWords
// whitespace-separated words or, for unspaced text (CJK), MinCheckLetters letters.
const (
	MinCheckWords   = 3
	MinCheckLetters = 4
)

// TextDetector guesses the language of text; ok=false when undecided.
// *langid.Identifier satisfies it.
type TextDetector interface {
	Detect(text string) (lang string, ok bool)
}

type Resolved struct {
	Lang, Text string
	Segments   []Segment // segments of the pass whose text is returned
}

type Resolver struct {
	T                            Transcriber
	ID                           TextDetector
	Pair                         [2]string
	LogprobFloor, MaxCompression float64
}

func longEnough(text string) bool {
	words := strings.Fields(text)
	if len(words) >= MinCheckWords {
		return true
	}
	if len(words) != 1 {
		return false
	}
	n := 0
	for _, r := range words[0] {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n >= MinCheckLetters
}

// says reports whether the text is confidently in lang (length rule applied).
func (r *Resolver) says(text, lang string) bool {
	if r.ID == nil || !longEnough(text) {
		return false
	}
	l, ok := r.ID.Detect(text)
	return ok && l == lang
}

// noise reports whether a pass result is unusable (empty, low confidence or
// repetitive).
func (r *Resolver) noise(res Result) bool {
	return strings.TrimSpace(res.Text) == "" || res.AvgLogprob < r.LogprobFloor || res.CompressionRatio > r.MaxCompression
}

func (r *Resolver) check(res Result, lang string) (Resolved, error) {
	if r.noise(res) {
		return Resolved{}, ErrNoise
	}
	return Resolved{Lang: lang, Text: strings.TrimSpace(res.Text), Segments: res.Segments}, nil
}

type passOut struct {
	res Result
	err error
}

func (r *Resolver) forced(ctx context.Context, wav []byte, lang string) <-chan passOut {
	ch := make(chan passOut, 1)
	go func() {
		res, err := r.T.Transcribe(ctx, wav, lang)
		ch <- passOut{res, err}
	}()
	return ch
}

func (r *Resolver) Resolve(ctx context.Context, wav16k []byte) (Resolved, error) {
	a, err := r.T.Transcribe(ctx, wav16k, "")
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Resolved{}, cerr
		}
		return Resolved{}, err
	}
	p := r.Pair
	if a.Language == p[0] || a.Language == p[1] {
		other := p[0]
		if a.Language == p[0] {
			other = p[1]
		}
		if r.says(a.Text, other) {
			bch := r.forced(ctx, wav16k, other)
			select {
			case <-ctx.Done():
				return Resolved{}, ctx.Err()
			case b := <-bch:
				if b.err == nil {
					return r.check(b.res, other)
				}
				if cerr := ctx.Err(); cerr != nil {
					return Resolved{}, cerr
				}
				// re-run failed: keep the first pass text, labelled by what it says
				return r.check(a, other)
			}
		}
		return r.check(a, a.Language)
	}

	// Outside the pair. If the auto pass already looks like noise, forcing a
	// language will not turn it into speech: skip the two extra requests.
	if r.noise(a) {
		return Resolved{}, ErrNoise
	}
	// Two forced passes in parallel.
	xch, ych := r.forced(ctx, wav16k, p[0]), r.forced(ctx, wav16k, p[1])
	var x, y passOut
	for got := 0; got < 2; got++ {
		select {
		case <-ctx.Done():
			return Resolved{}, ctx.Err()
		case x0 := <-xch:
			x, xch = x0, nil
		case y0 := <-ych:
			y, ych = y0, nil
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return Resolved{}, cerr
	}
	xok, yok := x.err == nil, y.err == nil
	switch {
	case !xok && !yok:
		return Resolved{}, y.err
	case xok && r.says(x.res.Text, p[1]):
		if yok {
			return r.check(y.res, p[1])
		}
		return r.check(x.res, p[1])
	case yok && r.says(y.res.Text, p[0]):
		if xok {
			return r.check(x.res, p[0])
		}
		return r.check(y.res, p[0])
	case !yok:
		return r.check(x.res, p[0])
	case !xok:
		return r.check(y.res, p[1])
	case y.res.AvgLogprob > x.res.AvgLogprob:
		return r.check(y.res, p[1])
	default:
		return r.check(x.res, p[0])
	}
}

// ResolveLocked transcribes with a single pass forced to lang, for recordings
// whose language is already decided. lang "" behaves exactly like Resolve.
func (r *Resolver) ResolveLocked(ctx context.Context, wav16k []byte, lang string) (Resolved, error) {
	if lang == "" {
		return r.Resolve(ctx, wav16k)
	}
	res, err := r.T.Transcribe(ctx, wav16k, lang)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Resolved{}, cerr
		}
		return Resolved{}, err
	}
	return r.check(res, lang)
}

// DetectTyped picks the language of typed text; falls back to Pair[0].
// No length rule: the user typed it, so use whatever the detector decides.
func (r *Resolver) DetectTyped(text string) string {
	if r.ID != nil {
		if l, ok := r.ID.Detect(text); ok && (l == r.Pair[0] || l == r.Pair[1]) {
			return l
		}
	}
	return r.Pair[0]
}
