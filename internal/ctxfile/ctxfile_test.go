package ctxfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNoHintSection(t *testing.T) {
	f := Parse("# Setting\nA\n")
	if f.Body != "# Setting\nA" {
		t.Errorf("Body = %q", f.Body)
	}
	if f.Hints != nil {
		t.Errorf("Hints = %v, want nil", f.Hints)
	}
}

func TestParseHintSection(t *testing.T) {
	f, err := Load("testdata/example.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.Hints["fr"], "Réunion entre Scaleway") {
		t.Errorf("fr hint = %q", f.Hints["fr"])
	}
	if !strings.HasPrefix(f.Hints["en"], "Meeting between Scaleway") {
		t.Errorf("en hint = %q", f.Hints["en"])
	}
	if !strings.Contains(f.Body, "## Terms") || strings.Contains(f.Body, "Whisper hint") {
		t.Errorf("Body = %q", f.Body)
	}
}

func TestParseHintSectionNotLast(t *testing.T) {
	f := Parse("# S\n## Whisper hint\n### fr\nBonjour\n## Notes\nX\n")
	if !strings.Contains(f.Body, "## Notes\nX") {
		t.Errorf("Body = %q", f.Body)
	}
	if f.Hints["fr"] != "Bonjour" {
		t.Errorf("fr = %q", f.Hints["fr"])
	}
}

func TestLoadBOMAndCRLF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.md")
	if err := os.WriteFile(p, []byte("\ufeff# S\r\n## Whisper hint\r\n### FR\r\nBonjour\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Hints["fr"] != "Bonjour" || f.Body != "# S" {
		t.Errorf("got %+v", f)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.md"))
	if err == nil || !strings.Contains(err.Error(), "context file") {
		t.Errorf("err = %v", err)
	}
}

func TestApproxTokens(t *testing.T) {
	f := File{Body: strings.Repeat("é", 8000)}
	if got := f.ApproxTokens(); got != 2000 {
		t.Errorf("got %d", got)
	}
}
