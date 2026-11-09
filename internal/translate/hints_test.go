package translate

import (
	"context"
	"strings"
	"testing"
)

var hintLangs = [2]string{"fr", "en"}

func TestWhisperHintsParsed(t *testing.T) {
	c := serve(t, 200, `{"fr":"Réunion Scaleway","en":"Scaleway meeting","de":"x"}`, nil)
	got, err := c.WhisperHints(context.Background(), "ctx", hintLangs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["fr"] != "Réunion Scaleway" || got["en"] != "Scaleway meeting" {
		t.Errorf("got %v", got)
	}
}

func TestWhisperHintsTruncated(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("mot ", 400))
	c := serve(t, 200, `{"fr":"`+long+`"}`, nil)
	got, err := c.WhisperHints(context.Background(), "ctx", hintLangs)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(got["fr"])); n != 150 {
		t.Errorf("words = %d, want 150", n)
	}
}

func TestWhisperHintsEmptyIsError(t *testing.T) {
	c := serve(t, 200, `{}`, nil)
	if _, err := c.WhisperHints(context.Background(), "ctx", hintLangs); err == nil {
		t.Fatal("want error")
	}
}

func TestWhisperHintsHTTPError(t *testing.T) {
	c := serve(t, 500, `boom`, nil)
	_, err := c.WhisperHints(context.Background(), "ctx", hintLangs)
	if err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Fatalf("err = %v", err)
	}
}
