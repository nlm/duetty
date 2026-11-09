// Package config loads duetty's settings from an optional TOML file, the
// environment and command-line flags (flags win over the file).
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/nlm/duetty/internal/usage"
)

// Config holds every duetty setting. Each field is a snake_case TOML key.
type Config struct {
	Languages      [2]string `toml:"languages"` // ISO-639-1, [0] = left pane
	APIURL         string    `toml:"api_url"`
	APIKey         string    `toml:"api_key"`
	STTModel       string    `toml:"stt_model"`
	TranslateModel string    `toml:"translate_model"`

	// Optional per-service endpoints, overriding APIURL (e.g. a local
	// Whisper server and a local Ollama).
	STTAPIURL       string `toml:"stt_api_url"`
	TranslateAPIURL string `toml:"translate_api_url"`
	// Optional per-service keys. api_key is sent only to api_url's own
	// server, never to a separate endpoint elsewhere.
	STTAPIKey       string `toml:"stt_api_key"`
	TranslateAPIKey string `toml:"translate_api_key"`

	TranslateContext int    `toml:"translate_context"`
	Timestamps       bool   `toml:"timestamps"`
	Mic              string `toml:"mic"`
	ONNXRuntimeLib   string `toml:"onnxruntime_lib"`

	VADModel           string  `toml:"vad_model"`
	VADThreshold       float64 `toml:"vad_threshold"`
	VADMinSpeechMS     float64 `toml:"vad_min_speech_ms"`
	VADEndSilenceMS    float64 `toml:"vad_end_silence_ms"`
	VADOnsetMS         float64 `toml:"vad_onset_ms"`
	VADPreSpeechMS     float64 `toml:"vad_pre_speech_ms"`
	VADTrimMarginMS    float64 `toml:"vad_trim_margin_ms"`
	SmartTurn          bool    `toml:"smart_turn"`
	SmartTurnModel     string  `toml:"smart_turn_model"`
	SmartTurnThreshold float64 `toml:"smart_turn_threshold"`
	SmartTurnPauseMS   float64 `toml:"smart_turn_pause_ms"`
	SmartTurnRecheckMS float64 `toml:"smart_turn_recheck_ms"`

	SmartTurnMaxSilenceMS float64 `toml:"smart_turn_max_silence_ms"`

	Denoise      bool   `toml:"denoise"`
	DenoiseModel string `toml:"denoise_model"`

	NoiseMinLogprob     float64 `toml:"noise_min_logprob"`
	NoiseMaxCompression float64 `toml:"noise_max_compression"`

	LivePreview           bool    `toml:"live_preview"`
	LivePreviewIntervalMS float64 `toml:"live_preview_interval_ms"`

	// Optional prices for the session cost estimate; no defaults. The
	// estimate is shown only when all three are set.
	PriceAudioPerMin *float64 `toml:"price_audio_per_min"`
	PriceInPerMTok   *float64 `toml:"price_in_per_mtok"`
	PriceOutPerMTok  *float64 `toml:"price_out_per_mtok"`
	PriceCurrency    string   `toml:"price_currency"`

	LogFile string `toml:"log_file"` // "" = no log

	ContextFile     string `toml:"context_file"` // "" = no context file
	WhisperHint     bool   `toml:"whisper_hint"`
	Correct         bool   `toml:"correct"`
	MarkCorrections bool   `toml:"mark_corrections"`
	// PrintWhisperHint is flag-only: print the Whisper hints and exit.
	PrintWhisperHint bool `toml:"-"`
}

// STTURL is the speech-to-text endpoint: stt_api_url, else api_url.
func (c Config) STTURL() string { return firstNonEmpty(c.STTAPIURL, c.APIURL) }

// TranslateURL is the translation endpoint: translate_api_url, else api_url.
func (c Config) TranslateURL() string { return firstNonEmpty(c.TranslateAPIURL, c.APIURL) }

// STTKey is the key sent to the speech-to-text endpoint (see serviceKey).
func (c Config) STTKey() string { return c.serviceKey(c.STTAPIURL, c.STTAPIKey) }

// TranslateKey is the key sent to the translation endpoint (see serviceKey).
func (c Config) TranslateKey() string { return c.serviceKey(c.TranslateAPIURL, c.TranslateAPIKey) }

// serviceKey picks the key for one service: its own key if set; else
// api_key, but only when the service uses api_url itself or another path on
// the same server (same scheme, host and port). api_key is never sent to a
// separate endpoint on another server, local or not: it was given for
// api_url's provider.
func (c Config) serviceKey(override, own string) string {
	if own != "" {
		return own
	}
	if override == "" || sameServer(override, c.APIURL) {
		return c.APIKey
	}
	return ""
}

// CheckKeys returns an error naming the first service that has no key while
// its endpoint is not on this machine (local servers usually take none). The
// error never contains a key.
func (c Config) CheckKeys() error {
	if err := c.CheckSTTKey(); err != nil {
		return err
	}
	return checkKey("translate", c.TranslateAPIURL, c.TranslateURL(), c.TranslateKey(), c.APIURL)
}

// CheckSTTKey is CheckKeys for the speech-to-text service only.
func (c Config) CheckSTTKey() error {
	return checkKey("stt", c.STTAPIURL, c.STTURL(), c.STTKey(), c.APIURL)
}

func checkKey(svc, override, effective, key, apiURL string) error {
	if key != "" || isLocalURL(effective) {
		return nil
	}
	if override != "" && !sameServer(override, apiURL) {
		return fmt.Errorf("no API key for %[1]s_api_url: set %[1]s_api_key or DUETTY_%[2]s_API_KEY (api_key is only sent to api_url's server)",
			svc, strings.ToUpper(svc))
	}
	return errors.New("no API key: set DUETTY_API_KEY (or OPENAI_API_KEY), or api_key in the config file")
}

// sameServer reports whether a and b have the same scheme, host and port.
func sameServer(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && b != "" &&
		strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func isLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && isLocalHost(u.Hostname())
}

func isLocalHost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// Prices returns the configured prices, or nil unless all three are set.
func (c Config) Prices() *usage.Prices {
	if c.PriceAudioPerMin == nil || c.PriceInPerMTok == nil || c.PriceOutPerMTok == nil {
		return nil
	}
	return &usage.Prices{
		AudioPerMin: *c.PriceAudioPerMin,
		InPerMTok:   *c.PriceInPerMTok,
		OutPerMTok:  *c.PriceOutPerMTok,
		Currency:    c.PriceCurrency,
	}
}

// userConfigPath returns <user config dir>/duetty/config.toml, derived from
// XDG_CONFIG_HOME or HOME via getenv, or "" if neither is set.
func userConfigPath(getenv func(string) string) string {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "duetty", "config.toml")
}

// Defaults returns the built-in configuration. APIURL has no default: the
// default models are Scaleway names, and silently sending a Scaleway key to
// another provider must not happen, so the endpoint is always explicit.
func Defaults() Config {
	return Config{
		Languages:             [2]string{"fr", "en"},
		STTModel:              "whisper-large-v3",
		TranslateModel:        "mistral-small-3.2-24b-instruct-2506",
		TranslateContext:      4,
		VADThreshold:          0.6,
		VADMinSpeechMS:        250,
		VADEndSilenceMS:       900,
		VADOnsetMS:            200,
		VADPreSpeechMS:        500,
		VADTrimMarginMS:       150,
		SmartTurn:             true,
		SmartTurnThreshold:    0.5,
		SmartTurnPauseMS:      200,
		SmartTurnRecheckMS:    600,
		SmartTurnMaxSilenceMS: 2500,
		NoiseMinLogprob:       -1.0,
		NoiseMaxCompression:   2.4,
		LivePreview:           true,
		LivePreviewIntervalMS: 1000,
		WhisperHint:           false,
		Correct:               true,
		MarkCorrections:       true,
	}
}

// UsageError wraps a command-line parse error (unknown flag, bad value).
// The caller should print it together with the usage text.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// flagVars are the targets of the command-line flags.
type flagVars struct {
	path, langs string
	tmp         Config // copied into the result only for flags explicitly set
}

// newFlagSet defines every duetty flag, with defaults taken from v.tmp. Its
// output is discarded; Usage prints the help text.
func newFlagSet(v *flagVars) *flag.FlagSet {
	fs := flag.NewFlagSet("duetty", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	t := &v.tmp
	fs.StringVar(&v.path, "config", "", "path to a TOML config file (default: ~/.config/duetty/config.toml, or $XDG_CONFIG_HOME/duetty/config.toml, if present; ./config.toml is read only via -config)")
	fs.StringVar(&v.langs, "languages", "", "two language codes, left pane first, e.g. fr,en (default fr,en)")
	fs.StringVar(&v.langs, "langs", "", "short for -languages")
	fs.StringVar(&t.APIURL, "api-url", t.APIURL, "OpenAI-compatible API URL (required; or env DUETTY_API_URL / OPENAI_BASE_URL), e.g. https://api.scaleway.ai/<project_id>/v1")
	fs.StringVar(&t.STTAPIURL, "stt-api-url", t.STTAPIURL, "speech-to-text endpoint, if not api_url (e.g. a local Whisper server)")
	fs.StringVar(&t.TranslateAPIURL, "translate-api-url", t.TranslateAPIURL, "translation endpoint, if not api_url (e.g. a local Ollama)")
	fs.StringVar(&t.STTModel, "stt-model", t.STTModel, "speech-to-text model")
	fs.StringVar(&t.TranslateModel, "translate-model", t.TranslateModel, "translation model")
	fs.IntVar(&t.TranslateContext, "translate-context", t.TranslateContext, "previous sentences given to the translator as context")
	fs.BoolVar(&t.Timestamps, "timestamps", t.Timestamps, "show times next to sentences")
	fs.StringVar(&t.Mic, "mic", t.Mic, "microphone device name; \"off\" disables audio (typed input only)")
	fs.StringVar(&t.ONNXRuntimeLib, "onnxruntime-lib", t.ONNXRuntimeLib, "path to the onnxruntime shared library")
	fs.StringVar(&t.VADModel, "vad-model", t.VADModel, "path to the Silero voice activity model")
	fs.Float64Var(&t.VADThreshold, "vad-threshold", t.VADThreshold, "speech probability threshold, between 0 and 1")
	fs.Float64Var(&t.VADMinSpeechMS, "vad-min-speech-ms", t.VADMinSpeechMS, "shortest speech sent to transcription, in ms")
	fs.Float64Var(&t.VADEndSilenceMS, "vad-end-silence-ms", t.VADEndSilenceMS, "silence that ends a turn when Smart Turn is off, in ms")
	fs.BoolVar(&t.SmartTurn, "smart-turn", t.SmartTurn, "use Smart Turn end-of-turn detection")
	fs.StringVar(&t.SmartTurnModel, "smart-turn-model", t.SmartTurnModel, "path to the Smart Turn model")
	fs.BoolVar(&t.Denoise, "denoise", t.Denoise, "local noise suppression before voice detection and transcription")
	fs.StringVar(&t.DenoiseModel, "denoise-model", t.DenoiseModel, "path to the denoise model")
	fs.BoolVar(&t.LivePreview, "live-preview", t.LivePreview, "show provisional text and translation while speaking; -live-preview=false turns it off (it re-sends audio, raising Whisper cost)")
	fs.StringVar(&t.LogFile, "log-file", t.LogFile, "write a timings/errors log (never conversation text) to this file")
	fs.StringVar(&t.ContextFile, "context", t.ContextFile, "path to a context file (Markdown) describing the conversation (people, terms), sent with every translation")
	fs.BoolVar(&t.WhisperHint, "whisper-hint", t.WhisperHint, "send per-language hints from the context file to Whisper; -whisper-hint=false turns it off")
	fs.BoolVar(&t.Correct, "correct", t.Correct, "fix misheard words in the speaker's text (uses the context file if given); -correct=false translates the text as heard")
	fs.BoolVar(&t.MarkCorrections, "mark-corrections", t.MarkCorrections, "show corrected words in blue; -mark-corrections=false turns it off")
	fs.BoolVar(&t.PrintWhisperHint, "print-whisper-hint", t.PrintWhisperHint, "print the Whisper hints for -context and exit")
	return fs
}

// Usage writes the command-line help (every flag with its default) to w.
func Usage(w io.Writer) {
	v := &flagVars{tmp: Defaults()}
	fs := newFlagSet(v)
	fs.SetOutput(w)
	fmt.Fprintf(w, "Usage: duetty [flags]\n\nFlags:\n")
	fs.PrintDefaults()
}

// Load builds a Config: defaults, then the TOML file (-config PATH, else the
// per-user <config dir>/duetty/config.toml -- $XDG_CONFIG_HOME/duetty/config.toml
// or ~/.config/duetty/config.toml -- if it exists; ./config.toml is never read
// implicitly), then the environment for whatever the file left unset
// (api_key: DUETTY_API_KEY, then OPENAI_API_KEY; api_url: DUETTY_API_URL, then
// OPENAI_BASE_URL), then explicitly-set flags.
//
// -h/-help returns flag.ErrHelp; any other command-line parse error is
// returned as a *UsageError. Print Usage in both cases.
func Load(args []string, getenv func(string) string) (Config, error) {
	cfg := Defaults()
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	v := &flagVars{tmp: cfg}
	fs := newFlagSet(v)
	c, tmp := &cfg, &v.tmp
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, err
		}
		return Config{}, &UsageError{Err: err}
	}
	path, langs := v.path, v.langs
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// File.
	// Without -config only the per-user file is auto-loaded; ./config.toml is
	// never read implicitly (an untrusted directory could redirect api_url).
	explicit := set["config"]
	if !explicit {
		if p := userConfigPath(getenv); p != "" {
			if _, err := os.Stat(p); err == nil {
				path, explicit = p, true
			}
		}
	}
	if explicit {
		md, err := toml.DecodeFile(path, c)
		if err != nil {
			return Config{}, fmt.Errorf("config file %s: %w", path, err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return Config{}, fmt.Errorf("config file %s: unknown key %s", path, u[0])
		}
	}

	if cfg.APIKey == "" {
		cfg.APIKey = firstEnv(getenv, "DUETTY_API_KEY", "OPENAI_API_KEY")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = firstEnv(getenv, "DUETTY_API_URL", "OPENAI_BASE_URL")
	}
	if cfg.STTAPIKey == "" {
		cfg.STTAPIKey = getenv("DUETTY_STT_API_KEY")
	}
	if cfg.TranslateAPIKey == "" {
		cfg.TranslateAPIKey = getenv("DUETTY_TRANSLATE_API_KEY")
	}

	// Flags override, only those explicitly set.
	over := map[string]func(){
		"api-url":            func() { cfg.APIURL = tmp.APIURL },
		"stt-api-url":        func() { cfg.STTAPIURL = tmp.STTAPIURL },
		"translate-api-url":  func() { cfg.TranslateAPIURL = tmp.TranslateAPIURL },
		"stt-model":          func() { cfg.STTModel = tmp.STTModel },
		"translate-model":    func() { cfg.TranslateModel = tmp.TranslateModel },
		"translate-context":  func() { cfg.TranslateContext = tmp.TranslateContext },
		"timestamps":         func() { cfg.Timestamps = tmp.Timestamps },
		"mic":                func() { cfg.Mic = tmp.Mic },
		"onnxruntime-lib":    func() { cfg.ONNXRuntimeLib = tmp.ONNXRuntimeLib },
		"vad-model":          func() { cfg.VADModel = tmp.VADModel },
		"vad-threshold":      func() { cfg.VADThreshold = tmp.VADThreshold },
		"vad-min-speech-ms":  func() { cfg.VADMinSpeechMS = tmp.VADMinSpeechMS },
		"vad-end-silence-ms": func() { cfg.VADEndSilenceMS = tmp.VADEndSilenceMS },
		"smart-turn":         func() { cfg.SmartTurn = tmp.SmartTurn },
		"smart-turn-model":   func() { cfg.SmartTurnModel = tmp.SmartTurnModel },
		"denoise":            func() { cfg.Denoise = tmp.Denoise },
		"denoise-model":      func() { cfg.DenoiseModel = tmp.DenoiseModel },
		"live-preview":       func() { cfg.LivePreview = tmp.LivePreview },
		"log-file":           func() { cfg.LogFile = tmp.LogFile },
		"context":            func() { cfg.ContextFile = tmp.ContextFile },
		"whisper-hint":       func() { cfg.WhisperHint = tmp.WhisperHint },
		"correct":            func() { cfg.Correct = tmp.Correct },
		"mark-corrections":   func() { cfg.MarkCorrections = tmp.MarkCorrections },
		"print-whisper-hint": func() { cfg.PrintWhisperHint = tmp.PrintWhisperHint },
	}
	for name, apply := range over {
		if set[name] {
			apply()
		}
	}
	if set["languages"] && set["langs"] {
		return Config{}, &UsageError{Err: errors.New("-languages and -langs are the same flag; give one")}
	}
	if set["languages"] || set["langs"] {
		parts := strings.Split(langs, ",")
		if len(parts) != 2 {
			return Config{}, errors.New("languages: need exactly two")
		}
		cfg.Languages = [2]string{strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// firstEnv returns the first non-empty value among the named variables.
func firstEnv(getenv func(string) string, names ...string) string {
	for _, n := range names {
		if v := getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// Validate checks the configuration. Errors never include the API key.
func (c Config) Validate() error {
	a, b := c.Languages[0], c.Languages[1]
	switch {
	case a == "" || b == "":
		return errors.New("languages: need exactly two")
	case a == b:
		return fmt.Errorf("languages: %s and %s are the same", a, b)
	case !WhisperSupported(a):
		return fmt.Errorf("languages: unsupported code %s", a)
	case !WhisperSupported(b):
		return fmt.Errorf("languages: unsupported code %s", b)
	}
	if c.TranslateContext < 0 {
		return fmt.Errorf("translate_context: must be >= 0, got %d", c.TranslateContext)
	}
	if !(c.VADThreshold > 0 && c.VADThreshold < 1) {
		return fmt.Errorf("vad_threshold: must be between 0 and 1 (exclusive), got %g", c.VADThreshold)
	}
	if !(c.SmartTurnThreshold > 0 && c.SmartTurnThreshold < 1) {
		return fmt.Errorf("smart_turn_threshold: must be between 0 and 1 (exclusive), got %g", c.SmartTurnThreshold)
	}
	for name, v := range map[string]float64{
		"vad_min_speech_ms": c.VADMinSpeechMS, "vad_end_silence_ms": c.VADEndSilenceMS,
		"vad_onset_ms": c.VADOnsetMS, "vad_pre_speech_ms": c.VADPreSpeechMS, "vad_trim_margin_ms": c.VADTrimMarginMS,
		"smart_turn_pause_ms": c.SmartTurnPauseMS, "smart_turn_recheck_ms": c.SmartTurnRecheckMS,
		"smart_turn_max_silence_ms": c.SmartTurnMaxSilenceMS,
	} {
		if !(v >= 0) || math.IsInf(v, 0) {
			return fmt.Errorf("%s: must be >= 0, got %g", name, v)
		}
	}
	for name, v := range map[string]float64{
		"noise_min_logprob": c.NoiseMinLogprob, "noise_max_compression": c.NoiseMaxCompression,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s: must be a finite number, got %g", name, v)
		}
	}
	if !(c.LivePreviewIntervalMS >= 100) || math.IsInf(c.LivePreviewIntervalMS, 0) {
		return fmt.Errorf("live_preview_interval_ms: must be >= 100, got %g", c.LivePreviewIntervalMS)
	}
	for name, p := range map[string]*float64{
		"price_audio_per_min": c.PriceAudioPerMin, "price_in_per_mtok": c.PriceInPerMTok,
		"price_out_per_mtok": c.PriceOutPerMTok,
	} {
		if p != nil && (!(*p >= 0) || math.IsInf(*p, 0)) {
			return fmt.Errorf("%s: must be a finite number >= 0, got %g", name, *p)
		}
	}
	if c.STTURL() == "" || c.TranslateURL() == "" {
		return errors.New("api_url: required: set api_url in the config file, -api-url, or DUETTY_API_URL (e.g. https://api.scaleway.ai/<project_id>/v1)")
	}
	for _, e := range []struct{ name, url string }{
		{"api_url", c.APIURL}, {"stt_api_url", c.STTAPIURL}, {"translate_api_url", c.TranslateAPIURL},
	} {
		if e.url == "" {
			continue
		}
		if err := validateAPIURL(e.url); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	return nil
}

func validateAPIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("not a valid URL")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLocalHost(u.Hostname()):
		return nil
	}
	return errors.New("must be https (http is allowed only for localhost, 127.0.0.1 or ::1)")
}
