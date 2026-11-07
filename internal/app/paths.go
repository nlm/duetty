package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nlm/poly/internal/config"
	"github.com/nlm/poly/internal/denoise"
)

// Default locations, relative to the (symlink-resolved) executable's directory
// only, where scripts/setup-models.sh installs the models and runtime. The
// current directory is deliberately never searched: a planted native library
// or model there would be loaded (CWE-427).
const (
	defaultSileroModel    = "models/silero_vad.onnx"
	defaultSmartTurnModel = "models/smart-turn/smart-turn-v3.2-cpu.onnx"
	defaultONNXRuntimeLib = "third_party/onnxruntime/lib/libonnxruntime.so"
	// defaultDenoiseModel is installed by scripts/setup-denoise.sh.
	defaultDenoiseModel = "models/denoise/" + denoise.ModelFile
)

// pathEnv is the filesystem view used to resolve default paths (faked in tests).
type pathEnv struct {
	exeDir func() (string, error)
	stat   func(string) error
}

func osPathEnv() pathEnv {
	return pathEnv{
		exeDir: func() (string, error) {
			exe, err := os.Executable()
			if err != nil {
				return "", err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return "", err
			}
			return filepath.Dir(exe), nil
		},
		stat: func(p string) error { _, err := os.Stat(p); return err },
	}
}

// find returns rel under the executable's directory if it exists, and the
// candidates tried (none if the executable's directory is unknown).
func (e pathEnv) find(rel string) (string, []string) {
	d, err := e.exeDir()
	if err != nil || d == "" {
		return "", nil
	}
	p := filepath.Join(d, rel)
	if e.stat(p) == nil {
		return p, []string{p}
	}
	return "", []string{p}
}

// triedText describes the candidates for a message.
func triedText(tried []string) string {
	if len(tried) == 0 {
		return "executable directory unknown"
	}
	return "tried " + strings.Join(tried, ", ")
}

// resolveAudioPaths fills empty Silero / Smart Turn / denoise / onnxruntime paths from
// the default locations next to the executable. Explicit values are never rewritten. A missing
// Silero model is an error naming the paths tried; a missing Smart Turn or
// denoise model (when enabled) is a warning; a missing library stays "" (onnxruntime's own
// search).
func resolveAudioPaths(cfg config.Config, env pathEnv) (config.Config, []string, error) {
	var warns []string
	if cfg.ONNXRuntimeLib == "" {
		cfg.ONNXRuntimeLib, _ = env.find(defaultONNXRuntimeLib)
	}
	if cfg.SmartTurn && cfg.SmartTurnModel == "" {
		p, tried := env.find(defaultSmartTurnModel)
		if p == "" {
			warns = append(warns, fmt.Sprintf(
				"smart turn unavailable, using fixed silence: no smart_turn_model set and none next to the poly binary (%s); run scripts/setup-models.sh or set smart_turn_model / -smart-turn-model",
				triedText(tried)))
		}
		cfg.SmartTurnModel = p
	}
	if cfg.Denoise && cfg.DenoiseModel == "" {
		p, tried := env.find(defaultDenoiseModel)
		if p == "" {
			warns = append(warns, fmt.Sprintf(
				"denoise unavailable, continuing without it: no denoise_model set and none next to the poly binary (%s); run scripts/setup-denoise.sh or set denoise_model / -denoise-model",
				triedText(tried)))
		}
		cfg.DenoiseModel = p
	}
	if cfg.SileroModel == "" {
		p, tried := env.find(defaultSileroModel)
		if p == "" {
			return cfg, warns, fmt.Errorf(
				"silero VAD model not found next to the poly binary (%s): run scripts/setup-models.sh and `go build -o poly ./cmd/poly` in the repo root, set silero_vad_model / -silero-vad-model, or use -mic off",
				triedText(tried))
		}
		cfg.SileroModel = p
	}
	return cfg, warns, nil
}

// ResolveAudioPaths is resolveAudioPaths against the real executable
// directory, for other commands (poly-eval) that share poly's model layout.
func ResolveAudioPaths(cfg config.Config) (config.Config, []string, error) {
	return resolveAudioPaths(cfg, osPathEnv())
}
