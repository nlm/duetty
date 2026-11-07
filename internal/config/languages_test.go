package config

import "testing"

func TestLanguageCode(t *testing.T) {
	for in, want := range map[string]string{"fr": "fr", "FR": "fr", "french": "fr", "French": "fr", " Japanese ": "ja", "haitian creole": "ht"} {
		if got, ok := LanguageCode(in); !ok || got != want {
			t.Errorf("LanguageCode(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if got, ok := LanguageCode("klingon"); ok || got != "" {
		t.Errorf("klingon = %q,%v", got, ok)
	}
}

func TestEnglishName(t *testing.T) {
	for in, want := range map[string]string{"fr": "French", "en": "English", "ja": "Japanese", "ht": "Haitian creole", "xx": "xx"} {
		if got := EnglishName(in); got != want {
			t.Errorf("EnglishName(%q) = %q want %q", in, got, want)
		}
	}
}
