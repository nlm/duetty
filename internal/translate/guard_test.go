package translate

import (
	"testing"

	"github.com/nlm/duetty/internal/ctxfile"
)

func exampleContextWords(t *testing.T) map[string]bool {
	t.Helper()
	cf, err := ctxfile.Load("../ctxfile/testdata/example.md")
	if err != nil {
		t.Fatal(err)
	}
	return contextWords(cf.Body)
}

func TestCheckCorrection(t *testing.T) {
	words := exampleContextWords(t)
	for _, tc := range []struct {
		heard, corrected, checked string
		dropped                   bool
	}{
		{"Tom vient de se marier.", "Dan vient de se marier.", "Tom vient de se marier.", true},
		{"Tom is here.", "Théo is here.", "Tom is here.", true},
		{"Ferme le capot.", "Ferme le bucket.", "Ferme le capot.", true},
		{"scale way is great", "Scaleway is great", "Scaleway is great", false},
		{"J'avais de me reposer.", "J'avais besoin de me reposer.", "J'avais besoin de me reposer.", false},
		{"we use the storage", "we use the Scaleway storage", "we use the storage", true},
		{"the the bucket", "the bucket", "the bucket", false},
		{"ask Tom about scale way", "ask Dan about Scaleway", "ask Tom about Scaleway", true},
		// Known limit: cross-script fixes don't sound alike by letters, so
		// they are dropped and the heard text stays.
		{"今日はスケールウェイ", "今日はScaleway", "今日はスケールウェイ", true},
		// A replaced span must not smuggle in a context term or a name
		// that sounds like nothing heard there.
		{"we moved to scale way storage", "we moved to Scaleway Object Storage", "we moved to scale way Storage", true},
		{"the scale way migration", "the Scaleway Glacier migration", "the scale way migration", true},
		{"I talked to scale way", "I talked to Théo at Scaleway", "I talked to scale way", true},
		{"the north wind team", "Dan's Northwind team", "the north wind team", true},
		// Common words in the context file are not context terms.
		{"I need go", "I need to go", "I need to go", false},
		{"we moved car", "we moved the car", "we moved the car", false},
		// Equal-length replaced spans are judged word by word.
		{"Piense en su familia.", "Pense à sa famille.", "Piense en su familia.", true},
		{"their is a problem", "there is a problem", "there is a problem", false},
		{"I'll bee their", "I'll be there", "I'll be there", false},
	} {
		checked, dropped := checkCorrection(tc.heard, tc.corrected, words)
		if checked != tc.checked || dropped != tc.dropped {
			t.Errorf("checkCorrection(%q, %q) = %q, %v; want %q, %v", tc.heard, tc.corrected, checked, dropped, tc.checked, tc.dropped)
		}
	}
}

func TestContextWords(t *testing.T) {
	words := exampleContextWords(t)
	for _, w := range []string{"dan", "théo", "camille", "scaleway", "bucket", "glacier"} {
		if !words[w] {
			t.Errorf("%q missing from context words", w)
		}
	}
	if words["s3"] {
		t.Error("words under 3 runes must not be context words")
	}
}
