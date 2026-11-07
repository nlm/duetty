package langid

import "testing"

func TestDetectFrEn(t *testing.T) {
	id := New([2]string{"fr", "en"})
	for text, want := range map[string]string{
		"Nous en parlons constamment.": "fr",
		"I'll see if he's here.":       "en",
	} {
		if got, ok := id.Detect(text); !ok || got != want {
			t.Errorf("Detect(%q) = %q,%v; want %q,true", text, got, ok, want)
		}
	}
}

func TestDetectEsJa(t *testing.T) {
	id := New([2]string{"es", "ja"})
	for text, want := range map[string]string{
		"Esto es ridículo.": "es",
		"気持ちは分かる。":          "ja",
	} {
		if got, ok := id.Detect(text); !ok || got != want {
			t.Errorf("Detect(%q) = %q,%v; want %q,true", text, got, ok, want)
		}
	}
}

func TestDetectUnsupportedPair(t *testing.T) {
	for _, pair := range [][2]string{{"br", "en"}, {"fr", "unknown"}} {
		id := New(pair)
		if lang, ok := id.Detect("Nous en parlons constamment."); ok {
			t.Errorf("pair %v: got %q,true; want ok=false", pair, lang)
		}
	}
}

func TestDetectAmbiguous(t *testing.T) {
	id := New([2]string{"fr", "en"})
	if lang, ok := id.Detect("Taxi"); ok {
		t.Errorf("Detect(Taxi) = %q,true; want ok=false", lang)
	}
}

func TestDetectEmpty(t *testing.T) {
	id := New([2]string{"fr", "en"})
	if _, ok := id.Detect("  \n"); ok {
		t.Error("whitespace should be ok=false")
	}
}
