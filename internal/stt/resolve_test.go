package stt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nlm/poly/internal/langid"
)

type fakeT struct {
	mu    sync.Mutex
	res   map[string]Result
	errs  map[string]error
	calls []string
	block chan struct{} // if set, forced calls wait on it (ignoring ctx)
}

func (f *fakeT) Transcribe(ctx context.Context, wav []byte, lang string) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, lang)
	f.mu.Unlock()
	if f.block != nil && lang != "" {
		<-f.block
	}
	if e := f.errs[lang]; e != nil {
		return Result{}, e
	}
	return f.res[lang], nil
}

type fakeD map[string]struct {
	lang string
	ok   bool
}

func (d fakeD) Detect(text string) (string, bool) { v := d[text]; return v.lang, v.ok }

func det(m map[string]string) fakeD {
	d := fakeD{}
	for k, v := range m {
		d[k] = struct {
			lang string
			ok   bool
		}{v, true}
	}
	return d
}

const frText = "Nous en parlons constamment."

func newR(t Transcriber, d TextDetector) *Resolver {
	return &Resolver{T: t, ID: d, Pair: [2]string{"fr", "en"}, LogprobFloor: -1.0, MaxCompression: 2.4}
}

func TestAutoInPair(t *testing.T) {
	f := &fakeT{res: map[string]Result{"": {Text: "Bonjour.", Language: "fr", AvgLogprob: -0.2}}}
	got, err := newR(f, det(nil)).Resolve(context.Background(), nil)
	if err != nil || got != (Resolved{"fr", "Bonjour."}) || len(f.calls) != 1 {
		t.Fatalf("got %v %v calls=%v", got, err, f.calls)
	}
}

func TestAutoInPairTextSaysOther(t *testing.T) {
	f := &fakeT{res: map[string]Result{
		"":   {Text: frText, Language: "en", AvgLogprob: -0.2},
		"fr": {Text: frText, Language: "fr", AvgLogprob: -0.3},
	}}
	got, err := newR(f, det(map[string]string{frText: "fr"})).Resolve(context.Background(), nil)
	if err != nil || got.Lang != "fr" || len(f.calls) != 2 || f.calls[1] != "fr" {
		t.Fatalf("got %v %v calls=%v", got, err, f.calls)
	}
}

func TestAutoInPairTextSaysOtherRealLingua(t *testing.T) {
	f := &fakeT{res: map[string]Result{
		"":   {Text: frText, Language: "en", AvgLogprob: -0.2},
		"fr": {Text: frText, Language: "fr", AvgLogprob: -0.3},
	}}
	got, err := newR(f, langid.New([2]string{"fr", "en"})).Resolve(context.Background(), nil)
	if err != nil || got.Lang != "fr" {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestShortTextTrustsWhisper(t *testing.T) {
	// detector would (wrongly) say fr, but text is too short to be checked
	f := &fakeT{res: map[string]Result{"": {Text: "OK sure", Language: "en", AvgLogprob: -0.2}}}
	got, err := newR(f, det(map[string]string{"OK sure": "fr"})).Resolve(context.Background(), nil)
	if err != nil || got.Lang != "en" || len(f.calls) != 1 {
		t.Fatalf("got %v %v calls=%v", got, err, f.calls)
	}
}

func TestLongEnoughBoundary(t *testing.T) {
	cases := map[string]bool{
		"": false, "OK": false, "a b": false, "a b c": true, "  a  b  c ": true,
		"こんに": false, "こんにち": true, "日本語です": true,
	}
	for s, want := range cases {
		if got := longEnough(s); got != want {
			t.Errorf("longEnough(%q)=%v want %v", s, got, want)
		}
	}
}

func TestCJKBoundaryInResolve(t *testing.T) {
	for text, wantCalls := range map[string]int{"こんにち": 2, "こんに": 1} {
		f := &fakeT{res: map[string]Result{
			"":   {Text: text, Language: "en", AvgLogprob: -0.2},
			"fr": {Text: text, Language: "fr", AvgLogprob: -0.2},
		}}
		newR(f, det(map[string]string{text: "fr"})).Resolve(context.Background(), nil)
		if len(f.calls) != wantCalls {
			t.Errorf("%q calls=%v want %d", text, f.calls, wantCalls)
		}
	}
}

func TestAutoOutsidePairUsesForcedEcho(t *testing.T) {
	f := &fakeT{res: map[string]Result{
		"":   {Text: "xxx yyy zzz", Language: "pt", AvgLogprob: -0.2},
		"en": {Text: frText, Language: "en", AvgLogprob: -0.1},
		"fr": {Text: frText, Language: "fr", AvgLogprob: -0.4},
	}}
	got, err := newR(f, det(map[string]string{frText: "fr"})).Resolve(context.Background(), nil)
	if err != nil || got != (Resolved{"fr", frText}) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestOutsidePairEchoOtherPassFailed(t *testing.T) {
	f := &fakeT{
		res: map[string]Result{
			"":   {Text: "x", Language: "pt"},
			"en": {Text: frText, Language: "en", AvgLogprob: -0.1},
		},
		errs: map[string]error{"fr": errors.New("boom")},
	}
	got, err := newR(f, det(map[string]string{frText: "fr"})).Resolve(context.Background(), nil)
	if err != nil || got != (Resolved{"fr", frText}) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestOutsidePairEchoStillNoiseChecked(t *testing.T) {
	f := &fakeT{res: map[string]Result{
		"":   {Text: "x", Language: "pt"},
		"en": {Text: frText, AvgLogprob: -0.1},
		"fr": {Text: frText, AvgLogprob: -1.5},
	}}
	_, err := newR(f, det(map[string]string{frText: "fr"})).Resolve(context.Background(), nil)
	if !errors.Is(err, ErrNoise) {
		t.Fatalf("err=%v", err)
	}
}

func TestAutoOutsidePairLogprobTieBreak(t *testing.T) {
	f := &fakeT{res: map[string]Result{
		"":   {Text: "x", Language: "pt"},
		"fr": {Text: "aaa bbb ccc", AvgLogprob: -0.6},
		"en": {Text: "ddd eee fff", AvgLogprob: -0.3},
	}}
	got, err := newR(f, det(nil)).Resolve(context.Background(), nil)
	if err != nil || got != (Resolved{"en", "ddd eee fff"}) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestNoiseFloor(t *testing.T) {
	f := &fakeT{res: map[string]Result{"": {Text: "Thank you.", Language: "en", AvgLogprob: -1.3}}}
	if _, err := newR(f, det(nil)).Resolve(context.Background(), nil); !errors.Is(err, ErrNoise) {
		t.Fatalf("err=%v", err)
	}
}

func TestNoiseCompression(t *testing.T) {
	f := &fakeT{res: map[string]Result{"": {Text: "la la la", Language: "en", AvgLogprob: -0.2, CompressionRatio: 3.0}}}
	if _, err := newR(f, det(nil)).Resolve(context.Background(), nil); !errors.Is(err, ErrNoise) {
		t.Fatalf("err=%v", err)
	}
}

func TestNoiseEmpty(t *testing.T) {
	f := &fakeT{res: map[string]Result{"": {Text: "  ", Language: "en"}}}
	if _, err := newR(f, det(nil)).Resolve(context.Background(), nil); !errors.Is(err, ErrNoise) {
		t.Fatalf("err=%v", err)
	}
}

func TestOneForcedPassFails(t *testing.T) {
	f := &fakeT{
		res: map[string]Result{
			"":   {Text: "x", Language: "pt"},
			"en": {Text: "hello there friend", AvgLogprob: -0.3},
		},
		errs: map[string]error{"fr": errors.New("boom")},
	}
	got, err := newR(f, det(nil)).Resolve(context.Background(), nil)
	if err != nil || got != (Resolved{"en", "hello there friend"}) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestAllFail(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeT{errs: map[string]error{"": boom}}
	_, err := newR(f, det(nil)).Resolve(context.Background(), nil)
	if err == nil || errors.Is(err, ErrNoise) || len(f.calls) != 1 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
	// both forced fail
	f = &fakeT{res: map[string]Result{"": {Text: "x", Language: "pt"}}, errs: map[string]error{"fr": boom, "en": boom}}
	if _, err = newR(f, det(nil)).Resolve(context.Background(), nil); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}

func TestUnsupportedPairSkipsTextCheck(t *testing.T) {
	f := &fakeT{res: map[string]Result{"": {Text: "this is some text", Language: "en", AvgLogprob: -0.2}}}
	got, err := newR(f, fakeD{}).Resolve(context.Background(), nil)
	if err != nil || got.Lang != "en" || len(f.calls) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestCtxCancelDuringForcedPasses(t *testing.T) {
	f := &fakeT{res: map[string]Result{"": {Text: "x", Language: "pt"}}, block: make(chan struct{})}
	defer close(f.block)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := newR(f, det(nil)).Resolve(ctx, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoise) {
		t.Fatalf("err=%v", err)
	}
}

func TestDetectTyped(t *testing.T) {
	r := newR(nil, det(map[string]string{"hello there": "en"}))
	if got := r.DetectTyped("hello there"); got != "en" {
		t.Fatalf("got %q", got)
	}
	// short typed text still uses detector (no length rule)
	r = newR(nil, det(map[string]string{"hi": "en"}))
	if got := r.DetectTyped("hi"); got != "en" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectTypedFallback(t *testing.T) {
	if got := newR(nil, fakeD{}).DetectTyped("OK"); got != "fr" {
		t.Fatalf("got %q", got)
	}
}

// Auto-detect outside the pair but already noise: no forced passes.
func TestOutsidePairNoiseSkipsForcedPasses(t *testing.T) {
	for name, a := range map[string]Result{
		"empty":       {Text: "  ", Language: "pt"},
		"compression": {Text: "la la la la", Language: "pt", AvgLogprob: -0.2, CompressionRatio: 3.0},
		"logprob":     {Text: "obrigado", Language: "pt", AvgLogprob: -1.5},
	} {
		f := &fakeT{res: map[string]Result{
			"":   a,
			"fr": {Text: frText, AvgLogprob: -0.1},
			"en": {Text: "hello there friend", AvgLogprob: -0.1},
		}}
		_, err := newR(f, det(nil)).Resolve(context.Background(), nil)
		if !errors.Is(err, ErrNoise) || len(f.calls) != 1 {
			t.Errorf("%s: err=%v calls=%v, want ErrNoise after 1 call", name, err, f.calls)
		}
	}
}
