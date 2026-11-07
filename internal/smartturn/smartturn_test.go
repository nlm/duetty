package smartturn

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ortLib returns the onnxruntime shared library to test against, skipping
// when none is available (mirrors the other ONNX-backed packages: a plain
// `go test ./...` must stay hermetic on a machine without onnxruntime).
func ortLib(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SMART_TURN_TEST_ORT_LIB"); p != "" {
		return p
	}
	p, err := filepath.Abs("../../third_party/onnxruntime/lib/libonnxruntime.so")
	if err != nil {
		t.Skip("cannot resolve onnxruntime path")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("onnxruntime not installed (run scripts/setup-smart-turn.sh) or set SMART_TURN_TEST_ORT_LIB")
	}
	return p
}

func TestNewMissingModelErrors(t *testing.T) {
	lib := ortLib(t)
	d, err := New(lib, "/nonexistent.onnx", "cpu", 2)
	if err == nil {
		d.Close()
		t.Fatal("New with a missing model returned nil error")
	}
}

func TestPredictRange(t *testing.T) {
	model := os.Getenv("SMART_TURN_TEST_MODEL")
	if model == "" {
		t.Skip("set SMART_TURN_TEST_MODEL (absolute path to smart-turn-v3.2-cpu.onnx) to run")
	}
	d, err := New(ortLib(t), model, "cpu", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// 2 s of the synthetic tone at 48 kHz s16le.
	pcm := make([]byte, 2*48000*2)
	for n := 0; n < 2*48000; n++ {
		v := 0.3 * math.Sin(2*math.Pi*220*float64(n)/48000)
		binary.LittleEndian.PutUint16(pcm[2*n:], uint16(int16(v*32767)))
	}
	var p float32
	start := time.Now()
	for i := 0; i < 10; i++ {
		p, err = d.Predict(pcm)
		if err != nil {
			t.Fatal(err)
		}
		if p < 0 || p > 1 || p != p {
			t.Fatalf("probability %v outside [0,1]", p)
		}
	}
	t.Logf("p=%.4f, mean Predict latency %v over 10 calls", p, time.Since(start)/10)
}

// TestPredictAfterCloseErrors: the VAD listener's goroutine isn't joined at
// shutdown, so a held pause can still call Predict after the composition
// root's deferred Close -- that must be an error (the listener fails open),
// never a nil-session panic mid-cleanup. Hermetic: a zero Detector has the
// same nil session Close leaves behind, and Predict must check it before
// touching onnxruntime at all.
func TestPredictAfterCloseErrors(t *testing.T) {
	d := &Detector{}
	d.Close()
	if _, err := d.Predict(make([]byte, 2*4800)); !errors.Is(err, errClosed) {
		t.Fatalf("Predict after Close = %v, want errClosed", err)
	}
}

// TestPredictAfterCloseErrorsRealModel is the same against a real, loaded
// and then closed session -- the shutdown race the hermetic test models.
func TestPredictAfterCloseErrorsRealModel(t *testing.T) {
	model := os.Getenv("SMART_TURN_TEST_MODEL")
	if model == "" {
		t.Skip("set SMART_TURN_TEST_MODEL (absolute path to smart-turn-v3.2-cpu.onnx) to run")
	}
	d, err := New(ortLib(t), model, "cpu", 2)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := d.Predict(make([]byte, 2*48000)); !errors.Is(err, errClosed) {
		t.Fatalf("Predict after Close = %v, want errClosed", err)
	}
}
