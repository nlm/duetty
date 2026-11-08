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

	"github.com/nlm/duetty/internal/usage"
)

func noEnv(string) string { return "" }

const testURL = "https://api.scaleway.ai/p/v1"

// withURL prepends -api-url (required) so Load can succeed.
func withURL(args ...string) []string { return append([]string{"-api-url", testURL}, args...) }

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
	if c.APIURL != testURL || Defaults().APIURL != "" {
		t.Fatalf("api_url must have no default: %q", Defaults().APIURL)
	}
	if c.Languages != [2]string{"fr", "en"} || c.TranslateContext != 4 || c.VADThreshold != 0.6 ||
		c.VADMinSpeechMS != 250 || c.VADTrimMarginMS != 150 || !c.SmartTurn || c.Denoise || c.Timestamps ||
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
	os.MkdirAll(filepath.Join(xdg, "duetty"), 0o700)
	os.WriteFile(filepath.Join(xdg, "duetty", "config.toml"), []byte("translate_context = 9\n"), 0o600)
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
	os.WriteFile("config.toml", []byte("translate_context = 9\napi_url = \"https://evil.example\"\n"), 0o600)
	// Ignored: api_url stays unset (the file would have set it).
	_, err := Load(nil, noEnv)
	if err == nil || !strings.Contains(err.Error(), "api_url: required") {
		t.Fatalf("cwd config.toml must be ignored: %v", err)
	}
	c, err := Load(withURL(), noEnv)
	if err != nil || c.TranslateContext != 4 {
		t.Fatalf("cwd config.toml must be ignored: %+v %v", c, err)
	}
	c, err = Load([]string{"-config", "config.toml"}, noEnv)
	if err != nil || c.TranslateContext != 9 || c.APIURL != "https://evil.example" {
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
		_, err := Load([]string{"-api-url", u}, envMap(map[string]string{"OPENAI_API_KEY": "sekret"}))
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
		{"-vad-threshold", "NaN"}, {"-vad-min-speech-ms", "NaN"}, {"-vad-end-silence-ms", "NaN"},
	} {
		if _, err := Load(withURL(args...), noEnv); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
	c := Defaults()
	c.APIURL = testURL
	c.SmartTurnThreshold = math.NaN()
	if c.Validate() == nil {
		t.Error("NaN smart_turn_threshold accepted")
	}
}

func TestNoiseThresholdsMustBeFinite(t *testing.T) {
	ok := Defaults()
	ok.APIURL = testURL
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := ok
		c.NoiseMinLogprob = v
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "noise_min_logprob") {
			t.Errorf("noise_min_logprob=%g: err = %v", v, err)
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
	os.WriteFile(p, []byte("noise_min_logprob = nan\n"), 0o600)
	if _, err := Load(withURL("-config", p), noEnv); err == nil {
		t.Error("nan in file accepted")
	}
}

func TestAPIURLRequired(t *testing.T) {
	inTempDir(t)
	_, err := Load(nil, envMap(map[string]string{"OPENAI_API_KEY": "sekret"}))
	if err == nil || !strings.HasPrefix(err.Error(), "api_url: required") || strings.Contains(err.Error(), "sekret") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIEnvPrecedence(t *testing.T) {
	inTempDir(t)
	both := map[string]string{
		"DUETTY_API_KEY": "duetty-key", "OPENAI_API_KEY": "openai-key",
		"DUETTY_API_URL": "https://duetty.example/v1", "OPENAI_BASE_URL": "https://openai.example/v1",
	}
	c, err := Load(nil, envMap(both))
	if err != nil || c.APIKey != "duetty-key" || c.APIURL != "https://duetty.example/v1" {
		t.Fatalf("DUETTY_* must win: key %q url %q err %v", c.APIKey, c.APIURL, err)
	}
	c, err = Load(nil, envMap(map[string]string{"OPENAI_API_KEY": "openai-key", "OPENAI_BASE_URL": "https://openai.example/v1"}))
	if err != nil || c.APIKey != "openai-key" || c.APIURL != "https://openai.example/v1" {
		t.Fatalf("OPENAI_* fallback: key %q url %q err %v", c.APIKey, c.APIURL, err)
	}
	c, err = Load([]string{"-api-url", testURL}, envMap(both))
	if err != nil || c.APIURL != testURL {
		t.Fatalf("flag must win over env: %q %v", c.APIURL, err)
	}
	d := t.TempDir()
	p := filepath.Join(d, "c.toml")
	os.WriteFile(p, []byte("api_url = \"https://file.example/v1\"\n"), 0o600)
	c, err = Load([]string{"-config", p}, envMap(both))
	if err != nil || c.APIURL != "https://file.example/v1" {
		t.Fatalf("file must win over env: %q %v", c.APIURL, err)
	}
	if _, err := Load(nil, envMap(map[string]string{"DUETTY_API_URL": "http://evil.example/v1"})); err == nil {
		t.Fatal("plain http from env accepted")
	}
}

func TestLanguagesFlagAndAlias(t *testing.T) {
	inTempDir(t)
	for _, f := range []string{"-languages", "-langs"} {
		c, err := Load(withURL(f, "es,ja"), noEnv)
		if err != nil || c.Languages != [2]string{"es", "ja"} {
			t.Fatalf("%s: %v %v", f, c.Languages, err)
		}
	}
	_, err := Load(withURL("-languages", "es,ja", "-langs", "fr,en"), noEnv)
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("both flags: err = %v, want *UsageError", err)
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
	for _, want := range []string{"-languages", "-langs", "-api-url", "-config", "-mic", "-log-file", "(default 0.6)", "~/.config/duetty/config.toml"} {
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
		{"-vad-threshold", "1.5"},
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

func TestLivePreviewDefaultsOn(t *testing.T) {
	d := Defaults()
	if !d.LivePreview || d.LivePreviewIntervalMS != 1000 {
		t.Fatalf("LivePreview=%v LivePreviewIntervalMS=%v", d.LivePreview, d.LivePreviewIntervalMS)
	}
	if d.PriceAudioPerMin != nil || d.PriceInPerMTok != nil || d.PriceOutPerMTok != nil || d.Prices() != nil {
		t.Fatal("prices must have no defaults")
	}
}

func TestLivePreviewFlag(t *testing.T) {
	inTempDir(t)
	c, err := Load([]string{"-live-preview=false", "-api-url", "https://x"}, noEnv)
	if err != nil || c.LivePreview {
		t.Fatalf("-live-preview=false: LivePreview=%v err=%v", c.LivePreview, err)
	}
	c, err = Load([]string{"-live-preview", "-api-url", "https://x"}, noEnv)
	if err != nil || !c.LivePreview {
		t.Fatalf("-live-preview: LivePreview=%v err=%v", c.LivePreview, err)
	}
}

func TestLivePreviewOffInFile(t *testing.T) {
	d := inTempDir(t)
	p := filepath.Join(d, "c.toml")
	os.WriteFile(p, []byte("live_preview = false\n"), 0o600)
	c, err := Load(withURL("-config", p), noEnv)
	if err != nil || c.LivePreview {
		t.Fatalf("live_preview = false: LivePreview=%v err=%v", c.LivePreview, err)
	}
	// The flag overrides the file.
	c, err = Load(withURL("-config", p, "-live-preview"), noEnv)
	if err != nil || !c.LivePreview {
		t.Fatalf("file off, flag on: LivePreview=%v err=%v", c.LivePreview, err)
	}
}

func TestPricesFromFile(t *testing.T) {
	d := inTempDir(t)
	p := filepath.Join(d, "c.toml")
	os.WriteFile(p, []byte("price_audio_per_min = 0.003\nprice_in_per_mtok = 0.15\nprice_out_per_mtok = 0.35\nprice_currency = \"€\"\n"), 0o600)
	c, err := Load(withURL("-config", p), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := &usage.Prices{AudioPerMin: 0.003, InPerMTok: 0.15, OutPerMTok: 0.35, Currency: "€"}
	if got := c.Prices(); got == nil || *got != *want {
		t.Fatalf("Prices() = %+v, want %+v", got, want)
	}
	os.WriteFile(p, []byte("price_audio_per_min = 0.003\nprice_in_per_mtok = 0.15\n"), 0o600)
	c, err = Load(withURL("-config", p), noEnv)
	if err != nil || c.Prices() != nil {
		t.Fatalf("partial prices: %+v %v", c.Prices(), err)
	}
}

func TestValidatePreviewAndPrices(t *testing.T) {
	base := func() Config { c := Defaults(); c.APIURL = testURL; return c }
	f := func(v float64) *float64 { return &v }
	c := base()
	c.LivePreviewIntervalMS = 50
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "live_preview_interval_ms") {
		t.Errorf("interval 50: %v", err)
	}
	for name, set := range map[string]func(*Config, *float64){
		"price_audio_per_min": func(c *Config, v *float64) { c.PriceAudioPerMin = v },
		"price_in_per_mtok":   func(c *Config, v *float64) { c.PriceInPerMTok = v },
		"price_out_per_mtok":  func(c *Config, v *float64) { c.PriceOutPerMTok = v },
	} {
		for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
			c := base()
			set(&c, f(bad))
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%v: %v", name, bad, err)
			}
		}
		c := base()
		set(&c, f(0))
		if err := c.Validate(); err != nil {
			t.Errorf("%s=0 rejected: %v", name, err)
		}
	}
}

func TestPerServiceEndpoints(t *testing.T) {
	inTempDir(t)
	c, err := Load([]string{"-stt-api-url", "http://localhost:8000/v1", "-translate-api-url", "http://127.0.0.1:11434/v1"}, noEnv)
	if err != nil {
		t.Fatalf("both overrides, no api_url: %v", err)
	}
	if c.STTURL() != "http://localhost:8000/v1" || c.TranslateURL() != "http://127.0.0.1:11434/v1" || c.CheckKeys() != nil {
		t.Fatalf("stt %q translate %q keys %v", c.STTURL(), c.TranslateURL(), c.CheckKeys())
	}
	c, err = Load(withURL("-stt-api-url", "http://localhost:8000/v1"), noEnv)
	if err != nil || c.STTURL() != "http://localhost:8000/v1" || c.TranslateURL() != testURL || c.CheckKeys() == nil {
		t.Fatalf("one override: stt %q translate %q keys %v err %v", c.STTURL(), c.TranslateURL(), c.CheckKeys(), err)
	}
	if _, err := Load([]string{"-stt-api-url", "http://localhost:8000/v1"}, noEnv); err == nil || !strings.HasPrefix(err.Error(), "api_url: required") {
		t.Fatalf("translation endpoint missing: err = %v", err)
	}
	_, err = Load(withURL("-translate-api-url", "http://lan-box:11434/v1"), noEnv)
	if err == nil || !strings.HasPrefix(err.Error(), "translate_api_url: must be https") {
		t.Fatalf("plain http to another host: err = %v", err)
	}
}

// api_key was given for api_url's provider: it must never reach another
// server through stt_api_url / translate_api_url.
func TestAPIKeyScopedToAPIURLServer(t *testing.T) {
	inTempDir(t)
	env := envMap(map[string]string{"DUETTY_API_KEY": "scw"})
	c, err := Load(withURL(), env)
	if err != nil || c.STTKey() != "scw" || c.TranslateKey() != "scw" || c.CheckKeys() != nil {
		t.Fatalf("api_url only: %q %q %v %v", c.STTKey(), c.TranslateKey(), c.CheckKeys(), err)
	}
	// Same server, other path: still api_url's provider.
	c, _ = Load(withURL("-stt-api-url", "https://api.scaleway.ai/other/v1"), env)
	if c.STTKey() != "scw" {
		t.Fatalf("same server: stt key %q", c.STTKey())
	}
	// Another provider: no key, and a clear error.
	c, _ = Load(withURL("-stt-api-url", "https://api.other.example/v1"), env)
	if c.STTKey() != "" || c.TranslateKey() != "scw" {
		t.Fatalf("other provider got api_key: stt %q translate %q", c.STTKey(), c.TranslateKey())
	}
	if err := c.CheckKeys(); err == nil || !strings.Contains(err.Error(), "stt_api_key") || strings.Contains(err.Error(), "scw") {
		t.Fatalf("CheckKeys = %v", err)
	}
	// A local server does not get it either, and needs none.
	c, _ = Load(withURL("-translate-api-url", "http://localhost:11434/v1"), env)
	if c.TranslateKey() != "" || c.CheckKeys() != nil {
		t.Fatalf("local translate: key %q check %v", c.TranslateKey(), c.CheckKeys())
	}
	// Its own key, from the environment or the file.
	c, _ = Load(withURL("-stt-api-url", "https://api.other.example/v1"),
		envMap(map[string]string{"DUETTY_API_KEY": "scw", "DUETTY_STT_API_KEY": "other"}))
	if c.STTKey() != "other" || c.TranslateKey() != "scw" || c.CheckKeys() != nil {
		t.Fatalf("own stt key: %q %q %v", c.STTKey(), c.TranslateKey(), c.CheckKeys())
	}
	c, _ = Load(withURL("-translate-api-url", "https://api.other.example/v1"),
		envMap(map[string]string{"DUETTY_API_KEY": "scw", "DUETTY_TRANSLATE_API_KEY": "tr"}))
	if c.TranslateKey() != "tr" || c.STTKey() != "scw" {
		t.Fatalf("own translate key: %q %q", c.TranslateKey(), c.STTKey())
	}
}
