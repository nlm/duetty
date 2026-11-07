package app

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlm/duetty/internal/config"
)

// fakePathEnv: exe dir /opt/duetty; only paths in present exist. /work stands
// for the current directory, which must never be searched.
func fakePathEnv(present ...string) pathEnv {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return pathEnv{
		exeDir: func() (string, error) { return "/opt/duetty", nil },
		stat: func(p string) error {
			if set[p] {
				return nil
			}
			return fs.ErrNotExist
		},
	}
}

func TestResolvePathsExeDirWinsOverCwd(t *testing.T) {
	var all []string
	for _, dir := range []string{"/opt/duetty", "/work"} { // /work present too: must not matter
		all = append(all,
			filepath.Join(dir, defaultSileroModel),
			filepath.Join(dir, defaultSmartTurnModel),
			filepath.Join(dir, defaultONNXRuntimeLib))
	}
	cfg := config.Defaults()
	got, warns, err := resolveAudioPaths(cfg, fakePathEnv(all...))
	if err != nil {
		t.Fatal(err)
	}
	if got.VADModel != "/opt/duetty/models/silero_vad.onnx" ||
		got.SmartTurnModel != "/opt/duetty/models/smart-turn/smart-turn-v3.2-cpu.onnx" ||
		got.ONNXRuntimeLib != "/opt/duetty/third_party/onnxruntime/lib/libonnxruntime.so" {
		t.Fatalf("got %+v", got)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings %v", warns)
	}
}

// CWE-427: files that exist only in the current directory are never used.
func TestResolvePathsIgnoresCwd(t *testing.T) {
	cfg := config.Defaults()
	got, warns, err := resolveAudioPaths(cfg, fakePathEnv(
		"/work/models/silero_vad.onnx",
		"/work/models/smart-turn/smart-turn-v3.2-cpu.onnx",
		"/work/third_party/onnxruntime/lib/libonnxruntime.so"))
	if err == nil || !strings.Contains(err.Error(), "/opt/duetty/models/silero_vad.onnx") {
		t.Fatalf("want error naming the exe-dir path, got %v", err)
	}
	if strings.Contains(err.Error(), "/work") {
		t.Fatalf("error mentions cwd: %v", err)
	}
	if got.ONNXRuntimeLib != "" || got.SmartTurnModel != "" {
		t.Fatalf("cwd candidates used: %+v", got)
	}
	if len(warns) != 1 || strings.Contains(warns[0], "/work") {
		t.Fatalf("warnings %v", warns)
	}
}

func TestResolvePathsExplicitUntouched(t *testing.T) {
	cfg := config.Defaults()
	cfg.VADModel = "/mine/silero.onnx" // does not even exist: not rewritten
	cfg.SmartTurnModel = "/mine/st.onnx"
	cfg.ONNXRuntimeLib = "/mine/libort.so"
	got, warns, err := resolveAudioPaths(cfg, fakePathEnv("/opt/duetty/models/silero_vad.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if got.VADModel != cfg.VADModel || got.SmartTurnModel != cfg.SmartTurnModel || got.ONNXRuntimeLib != cfg.ONNXRuntimeLib {
		t.Fatalf("explicit paths rewritten: %+v", got)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings %v", warns)
	}
}

func TestResolvePathsNoneFound(t *testing.T) {
	cfg := config.Defaults()
	got, warns, err := resolveAudioPaths(cfg, fakePathEnv())
	if got.ONNXRuntimeLib != "" {
		t.Errorf("library = %q, want \"\" (onnxruntime's own search)", got.ONNXRuntimeLib)
	}
	if err == nil {
		t.Fatal("want error when no Silero model is found")
	}
	for _, want := range []string{"/opt/duetty/models/silero_vad.onnx", "scripts/setup-models.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	// Smart Turn not found is only a warning naming the path tried.
	if len(warns) != 1 || !strings.Contains(warns[0], "/opt/duetty/models/smart-turn/smart-turn-v3.2-cpu.onnx") {
		t.Fatalf("want one smart turn warning naming paths, got %v", warns)
	}
}

func TestResolvePathsSmartTurnOffNoWarning(t *testing.T) {
	cfg := config.Defaults()
	cfg.SmartTurn = false
	_, warns, err := resolveAudioPaths(cfg, fakePathEnv("/opt/duetty/models/silero_vad.onnx"))
	if err != nil || len(warns) != 0 {
		t.Fatalf("err=%v warns=%v", err, warns)
	}
}

func TestResolvePathsExeDirError(t *testing.T) {
	env := fakePathEnv("/work/models/silero_vad.onnx")
	env.exeDir = func() (string, error) { return "", errors.New("no exe") }
	got, _, err := resolveAudioPaths(config.Defaults(), env)
	if err == nil || got.VADModel != "" || !strings.Contains(err.Error(), "executable directory unknown") {
		t.Fatalf("got %q err=%v", got.VADModel, err)
	}
}

func TestResolvePathsDenoise(t *testing.T) {
	cfg := config.Defaults()
	cfg.SmartTurn = false
	cfg.Denoise = true
	present := []string{"/opt/duetty/models/silero_vad.onnx", "/work/models/denoise/gtcrn_simple.onnx"}

	// Only next to the executable: a copy in the current directory is ignored.
	got, warns, err := resolveAudioPaths(cfg, fakePathEnv(present...))
	if err != nil {
		t.Fatal(err)
	}
	if got.DenoiseModel != "" || len(warns) != 1 ||
		!strings.Contains(warns[0], "denoise unavailable") ||
		!strings.Contains(warns[0], "/opt/duetty/models/denoise/gtcrn_simple.onnx") ||
		!strings.Contains(warns[0], "scripts/setup-denoise.sh") {
		t.Fatalf("model=%q warns=%v", got.DenoiseModel, warns)
	}

	got, warns, err = resolveAudioPaths(cfg, fakePathEnv(append(present, "/opt/duetty/models/denoise/gtcrn_simple.onnx")...))
	if err != nil || len(warns) != 0 || got.DenoiseModel != "/opt/duetty/models/denoise/gtcrn_simple.onnx" {
		t.Fatalf("model=%q warns=%v err=%v", got.DenoiseModel, warns, err)
	}

	// Explicit path untouched; denoise off: no lookup, no warning.
	cfg.DenoiseModel = "/mine/dn.onnx"
	if got, warns, _ = resolveAudioPaths(cfg, fakePathEnv(present...)); got.DenoiseModel != "/mine/dn.onnx" || len(warns) != 0 {
		t.Fatalf("explicit: model=%q warns=%v", got.DenoiseModel, warns)
	}
	cfg.DenoiseModel, cfg.Denoise = "", false
	if got, warns, _ = resolveAudioPaths(cfg, fakePathEnv(append(present, "/opt/duetty/models/denoise/gtcrn_simple.onnx")...)); got.DenoiseModel != "" || len(warns) != 0 {
		t.Fatalf("off: model=%q warns=%v", got.DenoiseModel, warns)
	}
}
