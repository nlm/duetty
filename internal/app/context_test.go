package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlm/duetty/internal/config"
	"github.com/nlm/duetty/internal/ctxfile"
)

type fakeGen struct {
	calls int
	body  string
	out   map[string]string
	err   error
}

func (g *fakeGen) WhisperHints(_ context.Context, body string, _ [2]string) (map[string]string, error) {
	g.calls++
	g.body = body
	return g.out, g.err
}

var testPair = [2]string{"fr", "en"}

func TestResolveHintsFromFile(t *testing.T) {
	g := &fakeGen{}
	f := ctxfile.File{Body: "x", Hints: map[string]string{"fr": "a", "en": "b"}}
	h, gen, err := ResolveHints(context.Background(), f, testPair, g)
	if err != nil || gen || g.calls != 0 || h["fr"] != "a" || h["en"] != "b" {
		t.Fatalf("h=%v gen=%v calls=%d err=%v", h, gen, g.calls, err)
	}
}

func TestResolveHintsGenerated(t *testing.T) {
	g := &fakeGen{out: map[string]string{"fr": "c", "en": "d"}}
	f := ctxfile.File{Body: "body", Hints: map[string]string{"fr": ""}}
	h, gen, err := ResolveHints(context.Background(), f, testPair, g)
	if err != nil || !gen || g.calls != 1 || g.body != "body" || h["fr"] != "c" {
		t.Fatalf("h=%v gen=%v calls=%d body=%q err=%v", h, gen, g.calls, g.body, err)
	}
}

func TestResolveHintsError(t *testing.T) {
	g := &fakeGen{err: errors.New("boom")}
	if _, _, err := ResolveHints(context.Background(), ctxfile.File{Body: "b"}, testPair, g); err == nil {
		t.Fatal("want error")
	}
}

func TestPrintWhisperHintsFromFileNeedsNoKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.md")
	src := "Body.\n\n## Whisper hint\n\n### fr\nbonjour\n\n### en\nhello\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.ContextFile = path
	cfg.Languages = [2]string{"fr", "en"}
	cfg.APIKey = ""
	var out bytes.Buffer
	if err := PrintWhisperHints(context.Background(), cfg, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "### fr\nbonjour") || !strings.Contains(got, "### en\nhello") {
		t.Errorf("output %q", got)
	}
}
