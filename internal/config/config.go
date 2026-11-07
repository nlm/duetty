// Package config loads poly's settings from an optional TOML file, the
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

	"github.com/nlm/poly/internal/usage"
)

// Config holds every poly setting. Each field is a snake_case TOML key.
type Config struct {
	Languages      [2]string `toml:"languages"` // ISO-639-1, [0] = left pane
	BaseURL        string    `toml:"base_url"`
	APIKey         string    `toml:"api_key"`
	STTModel       string    `toml:"stt_model"`
	TranslateModel string    `toml:"translate_model"`

	TranslateContext int    `toml:"translate_context"`
	Timestamps       bool   `toml:"timestamps"`
	MicDevice        string `toml:"mic_device"`
	ONNXRuntimeLib   string `toml:"onnxruntime_lib"`

	SileroModel        string  `toml:"silero_vad_model"`
	SileroThreshold    float64 `toml:"silero_vad_threshold"`
	VADMinSpeechMS     float64 `toml:"vad_min_speech_ms"`
	VADSilenceMS       float64 `toml:"vad_silence_ms"`
	VADOnsetMS         float64 `toml:"vad_onset_ms"`
	VADPrebufMS        float64 `toml:"vad_prebuf_ms"`
	TrimMarginMS       float64 `toml:"trim_margin_ms"`
	SmartTurn          bool    `toml:"smart_turn"`
	SmartTurnModel     string  `toml:"smart_turn_model"`
	SmartTurnThreshold float64 `toml:"smart_turn_threshold"`
	SmartTurnPauseMS   float64 `toml:"smart_turn_pause_ms"`
	SmartTurnRecheckMS float64 `toml:"smart_turn_recheck_ms"`

	SmartTurnMaxSilenceMS float64 `toml:"smart_turn_max_silence_ms"`

	Denoise      bool   `toml:"denoise"`
	DenoiseModel string `toml:"denoise_model"`

	NoiseLogprobFloor   float64 `toml:"noise_logprob_floor"`
	NoiseMaxCompression float64 `toml:"noise_max_compression"`

	LivePreview       bool    `toml:"live_preview"`
	PreviewIntervalMS float64 `toml:"preview_interval_ms"`

	// Optional prices for the session cost estimate; no defaults. The
	// estimate is shown only when all three are set.
	PriceAudioPerMin *float64 `toml:"price_audio_per_min"`
	PriceInPerMTok   *float64 `toml:"price_in_per_mtok"`
	PriceOutPerMTok  *float64 `toml:"price_out_per_mtok"`
	PriceCurrency    string   `toml:"price_currency"`

	LogFile string `toml:"log_file"` // "" = no log
}

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

// userConfigPath returns <user config dir>/poly/config.toml, derived from
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
	return filepath.Join(dir, "poly", "config.toml")
}

// Defaults returns the built-in configuration. BaseURL has no default: the
// default models are Scaleway names, and silently sending a Scaleway key to
// another provider must not happen, so the endpoint is always explicit.
func Defaults() Config {
	return Config{
		Languages:             [2]string{"fr", "en"},
		STTModel:              "whisper-large-v3",
		TranslateModel:        "mistral-small-3.2-24b-instruct-2506",
		TranslateContext:      4,
		SileroThreshold:       0.6,
		VADMinSpeechMS:        250,
		VADSilenceMS:          900,
		VADOnsetMS:            200,
		VADPrebufMS:           500,
		TrimMarginMS:          150,
		SmartTurn:             true,
		SmartTurnThreshold:    0.5,
		SmartTurnPauseMS:      200,
		SmartTurnRecheckMS:    600,
		SmartTurnMaxSilenceMS: 2500,
		NoiseLogprobFloor:     -1.0,
		NoiseMaxCompression:   2.4,
		LivePreview:           true,
		PreviewIntervalMS:     1000,
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

// newFlagSet defines every poly flag, with defaults taken from v.tmp. Its
// output is discarded; Usage prints the help text.
func newFlagSet(v *flagVars) *flag.FlagSet {
	fs := flag.NewFlagSet("poly", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	t := &v.tmp
	fs.StringVar(&v.path, "config", "", "path to a TOML config file (default: ~/.config/poly/config.toml, or $XDG_CONFIG_HOME/poly/config.toml, if present; ./config.toml is read only via -config)")
	fs.StringVar(&v.langs, "langs", "", "two language codes, left pane first, e.g. fr,en (default fr,en)")
	fs.StringVar(&t.BaseURL, "base-url", t.BaseURL, "OpenAI-compatible API base URL (required), e.g. https://api.scaleway.ai/<project_id>/v1")
	fs.StringVar(&t.STTModel, "stt-model", t.STTModel, "speech-to-text model")
	fs.StringVar(&t.TranslateModel, "translate-model", t.TranslateModel, "translation model")
	fs.IntVar(&t.TranslateContext, "translate-context", t.TranslateContext, "previous utterances given to the translator")
	fs.BoolVar(&t.Timestamps, "timestamps", t.Timestamps, "show timestamps")
	fs.StringVar(&t.MicDevice, "mic", t.MicDevice, "microphone device name; \"off\" disables audio (typed input only)")
	fs.StringVar(&t.ONNXRuntimeLib, "onnxruntime-lib", t.ONNXRuntimeLib, "path to the onnxruntime shared library")
	fs.StringVar(&t.SileroModel, "silero-vad-model", t.SileroModel, "path to the Silero VAD model")
	fs.Float64Var(&t.SileroThreshold, "silero-vad-threshold", t.SileroThreshold, "Silero speech probability threshold (0-1)")
	fs.Float64Var(&t.VADMinSpeechMS, "vad-min-speech-ms", t.VADMinSpeechMS, "minimum speech duration in ms")
	fs.Float64Var(&t.VADSilenceMS, "vad-silence-ms", t.VADSilenceMS, "trailing silence that ends an utterance, in ms")
	fs.BoolVar(&t.SmartTurn, "smart-turn", t.SmartTurn, "use Smart Turn end-of-utterance detection")
	fs.StringVar(&t.SmartTurnModel, "smart-turn-model", t.SmartTurnModel, "path to the Smart Turn model")
	fs.BoolVar(&t.Denoise, "denoise", t.Denoise, "denoise audio before transcription")
	fs.StringVar(&t.DenoiseModel, "denoise-model", t.DenoiseModel, "path to the denoise model")
	fs.BoolVar(&t.LivePreview, "live-preview", t.LivePreview, "show provisional text and translation while speaking; -live-preview=false turns it off (it re-sends audio, raising Whisper cost)")
	fs.StringVar(&t.LogFile, "log-file", t.LogFile, "write a timings/errors log (never conversation text) to this file")
	return fs
}

// Usage writes the command-line help (every flag with its default) to w.
func Usage(w io.Writer) {
	v := &flagVars{tmp: Defaults()}
	fs := newFlagSet(v)
	fs.SetOutput(w)
	fmt.Fprintf(w, "Usage: poly [flags]\n\nFlags:\n")
	fs.PrintDefaults()
}

// Load builds a Config: defaults, then the TOML file (-config PATH, else the
// per-user <config dir>/poly/config.toml -- $XDG_CONFIG_HOME/poly/config.toml
// or ~/.config/poly/config.toml -- if it exists; ./config.toml is never read
// implicitly), then OPENAI_API_KEY (when no key is set in the file), then
// explicitly-set flags.
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
	// never read implicitly (an untrusted directory could redirect base_url).
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
		cfg.APIKey = getenv("OPENAI_API_KEY")
	}

	// Flags override, only those explicitly set.
	over := map[string]func(){
		"base-url":             func() { cfg.BaseURL = tmp.BaseURL },
		"stt-model":            func() { cfg.STTModel = tmp.STTModel },
		"translate-model":      func() { cfg.TranslateModel = tmp.TranslateModel },
		"translate-context":    func() { cfg.TranslateContext = tmp.TranslateContext },
		"timestamps":           func() { cfg.Timestamps = tmp.Timestamps },
		"mic":                  func() { cfg.MicDevice = tmp.MicDevice },
		"onnxruntime-lib":      func() { cfg.ONNXRuntimeLib = tmp.ONNXRuntimeLib },
		"silero-vad-model":     func() { cfg.SileroModel = tmp.SileroModel },
		"silero-vad-threshold": func() { cfg.SileroThreshold = tmp.SileroThreshold },
		"vad-min-speech-ms":    func() { cfg.VADMinSpeechMS = tmp.VADMinSpeechMS },
		"vad-silence-ms":       func() { cfg.VADSilenceMS = tmp.VADSilenceMS },
		"smart-turn":           func() { cfg.SmartTurn = tmp.SmartTurn },
		"smart-turn-model":     func() { cfg.SmartTurnModel = tmp.SmartTurnModel },
		"denoise":              func() { cfg.Denoise = tmp.Denoise },
		"denoise-model":        func() { cfg.DenoiseModel = tmp.DenoiseModel },
		"live-preview":         func() { cfg.LivePreview = tmp.LivePreview },
		"log-file":             func() { cfg.LogFile = tmp.LogFile },
	}
	for name, apply := range over {
		if set[name] {
			apply()
		}
	}
	if set["langs"] {
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
	if !(c.SileroThreshold > 0 && c.SileroThreshold < 1) {
		return fmt.Errorf("silero_vad_threshold: must be between 0 and 1 (exclusive), got %g", c.SileroThreshold)
	}
	if !(c.SmartTurnThreshold > 0 && c.SmartTurnThreshold < 1) {
		return fmt.Errorf("smart_turn_threshold: must be between 0 and 1 (exclusive), got %g", c.SmartTurnThreshold)
	}
	for name, v := range map[string]float64{
		"vad_min_speech_ms": c.VADMinSpeechMS, "vad_silence_ms": c.VADSilenceMS,
		"vad_onset_ms": c.VADOnsetMS, "vad_prebuf_ms": c.VADPrebufMS, "trim_margin_ms": c.TrimMarginMS,
		"smart_turn_pause_ms": c.SmartTurnPauseMS, "smart_turn_recheck_ms": c.SmartTurnRecheckMS,
		"smart_turn_max_silence_ms": c.SmartTurnMaxSilenceMS,
	} {
		if !(v >= 0) || math.IsInf(v, 0) {
			return fmt.Errorf("%s: must be >= 0, got %g", name, v)
		}
	}
	for name, v := range map[string]float64{
		"noise_logprob_floor": c.NoiseLogprobFloor, "noise_max_compression": c.NoiseMaxCompression,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s: must be a finite number, got %g", name, v)
		}
	}
	if !(c.PreviewIntervalMS >= 100) || math.IsInf(c.PreviewIntervalMS, 0) {
		return fmt.Errorf("preview_interval_ms: must be >= 100, got %g", c.PreviewIntervalMS)
	}
	for name, p := range map[string]*float64{
		"price_audio_per_min": c.PriceAudioPerMin, "price_in_per_mtok": c.PriceInPerMTok,
		"price_out_per_mtok": c.PriceOutPerMTok,
	} {
		if p != nil && (!(*p >= 0) || math.IsInf(*p, 0)) {
			return fmt.Errorf("%s: must be a finite number >= 0, got %g", name, *p)
		}
	}
	return validateBaseURL(c.BaseURL)
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return errors.New("base_url: required (e.g. https://api.scaleway.ai/<project_id>/v1)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("base_url: not a valid URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return errors.New("base_url: must be https (http is allowed only for localhost, 127.0.0.1 or ::1)")
}
