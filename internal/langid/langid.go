// Package langid identifies which of two configured languages a text is in.
// It sanity-checks Whisper's language choice and classifies typed text.
package langid

import (
	"strings"

	lingua "github.com/pemistahl/lingua-go"
)

// minConfidence is the lowest confidence the top language needs for Detect
// to report a result.
const minConfidence = 0.7

// Identifier detects the language of a text among a fixed pair of languages.
// The underlying lingua detector is expensive to build (its two language
// models are preloaded), so it is built once in New; it is safe for concurrent use by multiple goroutines.
type Identifier struct {
	det lingua.LanguageDetector // nil when the pair is unsupported
}

// New builds an Identifier for the pair of ISO 639-1 codes. If lingua does not
// support either code (or the codes are identical), Detect always returns
// ok=false.
func New(pair [2]string) *Identifier {
	a := lingua.GetIsoCode639_1FromValue(pair[0])
	b := lingua.GetIsoCode639_1FromValue(pair[1])
	if a == lingua.UnknownIsoCode639_1 || b == lingua.UnknownIsoCode639_1 || a == b {
		return &Identifier{}
	}
	// Preload the two models (~0.1 s here) so the first Detect -- possibly a
	// typed line on the UI event loop -- does not stall on lazy loading.
	det := lingua.NewLanguageDetectorBuilder().FromIsoCodes639_1(a, b).WithPreloadedLanguageModels().Build()
	return &Identifier{det: det}
}

// Detect returns which of the pair the text is in. ok is false when the pair
// is unsupported, the text is blank, or the confidence is below 0.7.
func (i *Identifier) Detect(text string) (lang string, ok bool) {
	if i == nil || i.det == nil || strings.TrimSpace(text) == "" {
		return "", false
	}
	values := i.det.ComputeLanguageConfidenceValues(text)
	if len(values) == 0 || values[0].Value() < minConfidence {
		return "", false
	}
	return strings.ToLower(values[0].Language().IsoCode639_1().String()), true
}
