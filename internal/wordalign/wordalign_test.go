package wordalign

import (
	"reflect"
	"testing"
)

func TestTokensWideRunes(t *testing.T) {
	got := Tokens("今日は Scaleway  ok")
	want := []Token{{"今", true}, {"日", false}, {"は", false}, {"Scaleway", true}, {"ok", true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if s := Join(got); s != "今日は Scaleway ok" {
		t.Errorf("Join = %q", s)
	}
}

func TestAlignOps(t *testing.T) {
	a := Tokens("ask Tom about scale way and the the end")
	b := Tokens("Ask Dan about Scaleway and the end now")
	want := []Op{
		{Equal, 0, 1, 0, 1},
		{Replace, 1, 2, 1, 2},
		{Equal, 2, 3, 2, 3},
		{Replace, 3, 5, 3, 4},
		{Equal, 5, 6, 4, 5},
		{Equal, 6, 7, 5, 6},
		{Delete, 7, 8, 6, 6},
		{Equal, 8, 9, 6, 7},
		{Insert, 9, 9, 7, 8},
	}
	if got := Align(a, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestAlignPunctuationNeverMatches(t *testing.T) {
	got := Align(Tokens("a ,"), Tokens("a ,"))
	want := []Op{{Equal, 0, 1, 0, 1}, {Replace, 1, 2, 1, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
