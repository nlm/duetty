package ui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nlm/duetty/internal/convo"
)

const (
	tBase = "[gray]"
	tMark = "[blue]"
)

func TestCorrectionSegs(t *testing.T) {
	got := correctionSegs("scale way is great", "Scaleway is great", tBase, tMark)
	want := []seg{{tMark, "Scaleway", false}, {tBase, "is great", false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestCorrectionSegsCJK(t *testing.T) {
	got := correctionSegs("今日はスケールウェイ", "今日はScaleway", tBase, tMark)
	var marked []string
	for _, s := range got {
		if s.style == tMark {
			marked = append(marked, s.text)
		}
	}
	if !reflect.DeepEqual(marked, []string{"Scaleway"}) {
		t.Fatalf("marked %q in %#v", marked, got)
	}
}

func TestCorrectionSegsCaseOnly(t *testing.T) {
	got := correctionSegs("scaleway", "Scaleway", tBase, tMark)
	want := []seg{{tBase, "Scaleway", false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func corrRow(st convo.Status) convo.Row {
	r := row(1, st, "fr", "scale way", "x")
	r.Corrected = "Scaleway"
	return r
}

func TestLayoutMarksCorrection(t *testing.T) {
	l, _ := Layout([]convo.Row{corrRow(convo.Done)}, pair, 41, false, true)
	if !strings.Contains(l[0], "[blue]Scaleway") {
		t.Fatalf("no blue mark: %q", l[0])
	}
	l, _ = Layout([]convo.Row{corrRow(convo.Live)}, pair, 41, false, true)
	if !strings.Contains(l[0], "[blue::i]Scaleway") {
		t.Fatalf("no live blue mark: %q", l[0])
	}
}

func TestLayoutMarkOff(t *testing.T) {
	l, _ := Layout([]convo.Row{corrRow(convo.Done)}, pair, 41, false, false)
	if !strings.Contains(l[0], "Scaleway") || strings.Contains(l[0], "[blue]") {
		t.Fatalf("got %q", l[0])
	}
}

func TestLayoutCancelledShowsOriginal(t *testing.T) {
	l, _ := Layout([]convo.Row{corrRow(convo.Cancelled)}, pair, 41, false, true)
	if !strings.Contains(l[0], "scale way") || strings.Contains(l[0], "[blue") {
		t.Fatalf("got %q", l[0])
	}
}

func TestLayoutMarkKeepsText(t *testing.T) {
	for _, c := range [][2]string{
		{"今日はスケールウェイ", "今日はScaleway"},
		{"スケールウェイは", "Scalewayは"},
		{"スケールウェイ日本", "Scaleway日本"},
		{"scale way is great", "Scaleway is great"},
	} {
		r := row(1, convo.Done, "fr", c[0], "x")
		r.Corrected = c[1]
		on, _ := Layout([]convo.Row{r}, pair, 81, false, true)
		off, _ := Layout([]convo.Row{r}, pair, 81, false, false)
		if plain(on[0]) != plain(off[0]) {
			t.Errorf("%q: marked %q, unmarked %q", c[1], plain(on[0]), plain(off[0]))
		}
	}
}
