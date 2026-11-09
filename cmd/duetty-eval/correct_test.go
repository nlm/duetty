package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nlm/duetty/internal/translate"
)

func TestSameWords(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Bonjour, Camille !", "bonjour camille", true},
		{"we ship friday", "we ship Monday", false},
		{"今日は。", "今日は", true},
		{"a  b", "a\tb", true},
		{"神話からの引用らしいがよくわからない。", "神話からの引用らしいが、よくわからない。", true},
	} {
		if got := sameWords(c.a, c.b); got != c.want {
			t.Errorf("sameWords(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestMedian(t *testing.T) {
	if got := median(nil); got != 0 {
		t.Errorf("median(nil) = %v", got)
	}
	ms := time.Millisecond
	if got := median([]time.Duration{300 * ms, 100 * ms, 200 * ms}); got != 200*ms {
		t.Errorf("odd median = %v", got)
	}
	if got := median([]time.Duration{100 * ms, 200 * ms}); got != 150*ms {
		t.Errorf("even median = %v", got)
	}
}

type fakeTr struct{}

func (fakeTr) Translate(_ context.Context, r translate.Request) (translate.Result, error) {
	if r.Correct {
		return translate.Result{Translation: "t", Corrected: "fixed " + r.Text}, nil
	}
	return translate.Result{Translation: "t"}, nil
}

func TestCorrectLine(t *testing.T) {
	var st correctStats
	l := st.line(context.Background(), fakeTr{}, "a.wav", "hello world", "Hello, world.", "en", "fr")
	if !strings.Contains(l, "REPHRASE") {
		t.Errorf("want REPHRASE tag: %s", l)
	}
	l = st.line(context.Background(), fakeTr{}, "b.wav", "hello", "something else", "en", "fr")
	if strings.Contains(l, "REPHRASE") {
		t.Errorf("heard differs from ref, no tag expected: %s", l)
	}
	if st.heardOK != 1 || st.rephrased != 1 || len(st.plain) != 2 || len(st.correct) != 2 {
		t.Errorf("stats = %+v", st)
	}
	if !strings.HasPrefix(st.summary(), "rephrased 1/1 (clips heard correctly)\ndiscarded 0/1\nerrors 0\n") {
		t.Errorf("summary = %q", st.summary())
	}
}

type fixedTr struct {
	corrected string
	err       error
}

func (f fixedTr) Translate(_ context.Context, r translate.Request) (translate.Result, error) {
	if f.err != nil && r.Correct {
		return translate.Result{}, f.err
	}
	return translate.Result{Translation: "t", Corrected: f.corrected}, nil
}

func TestCorrectLineDiscardedAndUnchanged(t *testing.T) {
	var st correctStats
	l := st.line(context.Background(), fixedTr{corrected: ""}, "a", "hello", "Hello", "en", "fr")
	if !strings.Contains(l, "DISCARDED") || strings.Contains(l, "REPHRASE") {
		t.Errorf("line = %s", l)
	}
	l = st.line(context.Background(), fixedTr{corrected: "hello"}, "b", "hello", "Hello", "en", "fr")
	if strings.Contains(l, "DISCARDED") || strings.Contains(l, "REPHRASE") {
		t.Errorf("unchanged must carry no tag: %s", l)
	}
	if st.heardOK != 2 || st.discarded != 1 || st.rephrased != 0 {
		t.Errorf("stats = %+v", st)
	}
	if !strings.Contains(st.summary(), "discarded 1/2") {
		t.Errorf("summary = %q", st.summary())
	}
}

func TestCorrectLineErrorKeepsSamplesPaired(t *testing.T) {
	var st correctStats
	st.line(context.Background(), fixedTr{err: errors.New("boom")}, "a", "hello", "hello", "en", "fr")
	if st.errors != 1 || len(st.plain) != 0 || len(st.correct) != 0 || st.heardOK != 0 {
		t.Errorf("stats = %+v", st)
	}
	if !strings.Contains(st.summary(), "errors 1") {
		t.Errorf("summary = %q", st.summary())
	}
}
