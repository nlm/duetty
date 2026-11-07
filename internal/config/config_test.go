package config

import (
	"bytes"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

const testURL = "https://api.scaleway.ai/p/v1"

// withURL prepends -base-url (required) so Load can succeed.
func withURL(args ...string) []string { return append([]string{"-base-url", testURL}, args...) }

func inTempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Chdir(d)
	return d
}

func TestDefaults(t *testing.T) {
	inTempDir(t)
	c, err := Load(withURL(), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != testURL || Defaults().BaseURL != "" {
		t.Fatalf("base_url must have no default: %q", Defaults().BaseURL)
	}
	if c.Languages != [2]string{"fr", "en"} || c.TranslateContext != 4 || c.SileroThreshold != 0.6 ||
		c.VADMinSpeechMS != 250 || c.TrimMarginMS != 150 || !c.SmartTurn || c.Denoise || c.Timestamps ||
		c.TranslateModel != "mistral-small-3.2-24b-instruct-2506" || c.STTModel != "whisper-large-v3" {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestFlagOverridesFile(t *testing.T) {
	d := inTempDir(t)
	p := filepath.Join(d, "c.toml")
	os.WriteFile(p, []byte("languages = [\"es\",\"ja\"]\ntranslate_context = 7\n"), 0o600)
	c, err := Load(withURL("-config", p, "-langs", "fr,en"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Languages != [2]string{"fr", "en"} {
		t.Fatalf("langs = %v", c.Languages)
	}
	if c.TranslateContext != 7 {
		t.Fatalf("unset flag must not override file; got %d", c.TranslateContext)
	}
	c, err = Load(withURL("-config", p), noEnv)
	if err != nil || c.Languages != [2]string{"es", "ja"} {
		t.Fatalf("file langs: %v %v", c.Languages, err)
	}
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestUserConfigPickedUp(t *testing.T) {
	d := inTempDir(t)
	xdg := filepath.Join(d, "xdg")
	os.MkdirAll(filepath.Join(xdg, "poly"), 0o700)
	os.WriteFile(filepath.Join(xdg, "poly", "config.toml"), []byte("translate_context = 9\n"), 0o600)
	c, err := Load(withURL(), envMap(map[string]string{"XDG_CONFIG_HOME": xdg}))
	if err != nil || c.TranslateContext != 9 {
		t.Fatalf("%v %v", c.TranslateContext, err)
	}
	// HOME fallback.
	os.Rename(xdg, filepath.Join(d, ".config"))
	c, err = Load(withURL(), envMap(map[string]string{"HOME": d}))
	if err != nil || c.TranslateContext != 9 {
		t.Fatalf("HOME: %v %v", c.TranslateContext, err)
	}
}

func TestCwdConfigNotLoadedImplicitly(t *testing.T) {
	inTempDir(t)
	os.WriteFile("config.toml", []byte("translate_context = 9\nbase_url = \"https://evil.example\"\n"), 0o600)
	// Ignored: base_url stays unset (the file would have set it).
	_, err := Load(nil, noEnv)
	if err == nil || !strings.Contains(err.Error(), "base_url: required") {
		t.Fatalf("cwd config.toml must be ignored: %v", err)
	}
	c, err := Load(withURL(), noEnv)
	if err != nil || c.TranslateContext != 4 {
		t.Fatalf("cwd config.toml must be ignored: %+v %v", c, err)
	}
	c, err = Load([]string{"-config", "config.toml"}, noEnv)
	if err != nil || c.TranslateContext != 9 || c.BaseURL != "https://evil.example" {
		t.Fatalf("explicit -config must load: %v %v", c.TranslateContext, err)
	}
}

func TestBaseURLValidation(t *testing.T) {
	inTempDir(t)
	for u, ok := range map[string]bool{
		"https://api.scaleway.ai/x/v1": true, "http://localhost:8080/v1": true,
		"http://127.0.0.1:9/v1": true, "http://[::1]:9/v1": true,
		"http://example.com/v1": false, "ftp://localhost/": false,
		"://bad": false, "%zz": false, "https://": false,
	} {
		_, err := Load([]string{"-base-url", u}, envMap(map[string]string{"OPENAI_API_KEY": "sekret"}))
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", u, err, ok)
		}
		if err != nil && strings.Contains(err.Error(), "sekret") {
			t.Errorf("error leaks key: %v", err)
		}
	}
}

func TestRejectsNaN(t *testing.T) {
	inTempDir(t)
	for _, args := range [][]string{
		{"-silero-vad-threshold", "NaN"}, {"-vad-min-speech-ms", "NaN"}, {"-vad-silence-ms", "NaN"},
	} {
		if _, err := Load(withURL(args...), noEnv); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
	c := Defaults()
	c.BaseURL = testURL
	c.SmartTurnThreshold = math.NaN()
	if c.Validate() == nil {
		t.Error("NaN smart_turn_threshold accepted")
	}
}

func TestNoiseThresholdsMustBeFinite(t *testing.T) {
	ok := Defaults()
	ok.BaseURL = testURL
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := ok
		c.NoiseLogprobFloor = v
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "noise_logprob_floor") {
			t.Errorf("noise_logprob_floor=%g: err = %v", v, err)
		}
		c = ok
		c.NoiseMaxCompression = v
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "noise_max_compression") {
			t.Errorf("noise_max_compression=%g: err = %v", v, err)
		}
	}
	// Also through the TOML file (TOML has nan/inf literals).
	d := inTempDir(t)
	p := filepath.Join(d, "c.toml")
	os.WriteFile(p, []byte("noise_logprob_floor = nan\n"), 0o600)
	if _, err := Load(withURL("-config", p), noEnv); err == nil {
		t.Error("nan in file accepted")
	}
}

func TestBaseURLRequired(t *testing.T) {
	inTempDir(t)
	_, err := Load(nil, envMap(map[string]string{"OPENAI_API_KEY": "sekret"}))
	if err == nil || err.Error() != "base_url: required (e.g. https://api.scaleway.ai/<project_id>/v1)" {
		t.Fatalf("err = %v", err)
	}
}

func TestHelpAndUsage(t *testing.T) {
	inTempDir(t)
	for _, a := range []string{"-h", "-help", "--help"} {
		if _, err := Load([]string{a}, noEnv); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: err = %v, want flag.ErrHelp", a, err)
		}
	}
	_, err := Load([]string{"-nope"}, noEnv)
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "-nope") {
		t.Errorf("unknown flag: err = %v, want *UsageError", err)
	}
	if _, err := Load([]string{"-langs", "fr,fr"}, noEnv); errors.As(err, &ue) {
		t.Errorf("validation error must not be a UsageError: %v", err)
	}
	var b bytes.Buffer
	Usage(&b)
	out := b.String()
	for _, want := range []string{"-langs", "-base-url", "-config", "-mic", "-log-file", "(default 0.6)", "~/.config/poly/config.toml"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q:\n%s", want, out)
		}
	}
}

func TestMissingExplicitConfigIsError(t *testing.T) {
	inTempDir(t)
	if _, err := Load([]string{"-config", "nope.toml"}, noEnv); err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectsBadLanguages(t *testing.T) {
	inTempDir(t)
	for langs, want := range map[string]string{
		"fr":    "need exactly two",
		"fr,fr": "fr and fr are the same",
		"fr,xx": "unsupported code xx",
	} {
		_, err := Load(withURL("-langs", langs), noEnv)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("-langs %s: err = %v, want %q", langs, err, want)
		}
	}
}

func TestAPIKeyFromEnv(t *testing.T) {
	inTempDir(t)
	c, err := Load(withURL(), func(k string) string {
		if k == "OPENAI_API_KEY" {
			return "k"
		}
		return ""
	})
	if err != nil || c.APIKey != "k" {
		t.Fatalf("%q %v", c.APIKey, err)
	}
}

func TestRejectsBadNumbers(t *testing.T) {
	inTempDir(t)
	for _, args := range [][]string{
		{"-translate-context", "-1"},
		{"-silero-vad-threshold", "1.5"},
	} {
		if _, err := Load(withURL(args...), noEnv); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestLanguageNames(t *testing.T) {
	for code, want := range map[string]string{
		"fr": "Français", "en": "English", "es": "Español", "ja": "日本語", "de": "Deutsch",
		"it": "Italiano", "pt": "Português", "zh": "中文", "ko": "한국어", "ru": "Русский", "ar": "العربية",
		"sn": "Shona",
	} {
		if got := LanguageName(code); got != want {
			t.Errorf("%s = %q, want %q", code, got, want)
		}
	}
	if len(whisperLanguages) != 100 {
		t.Errorf("table has %d codes", len(whisperLanguages))
	}
	if !WhisperSupported("yue") || WhisperSupported("xx") {
		t.Error("WhisperSupported wrong")
	}
}
