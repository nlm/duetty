package denoise

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ortLib returns the onnxruntime library, skipping when it is absent so a
// plain `go test ./...` stays hermetic.
func ortLib(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("DENOISE_TEST_ORT_LIB"); p != "" {
		return p
	}
	p, _ := filepath.Abs("../../third_party/onnxruntime/lib/libonnxruntime.so")
	if _, err := os.Stat(p); err != nil {
		t.Skip("onnxruntime not installed (run scripts/setup-models.sh) or set DENOISE_TEST_ORT_LIB")
	}
	return p
}

// model returns the GTCRN model, skipping when it is absent.
func model(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("DENOISE_TEST_MODEL"); p != "" {
		return p
	}
	p, _ := filepath.Abs("../../models/denoise/" + ModelFile)
	if _, err := os.Stat(p); err != nil {
		t.Skip("denoise model not installed (run scripts/setup-denoise.sh) or set DENOISE_TEST_MODEL")
	}
	return p
}

func newDenoiser(t *testing.T) *Denoiser {
	t.Helper()
	lib, m := ortLib(t), model(t)
	d, err := New(lib, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func toPCM(x []float64) []byte {
	b := make([]byte, 2*len(x))
	for i, v := range x {
		s := math.Round(v * 32767)
		s = math.Max(-32768, math.Min(32767, s))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(s)))
	}
	return b
}

func fromPCM(b []byte) []float64 {
	out := make([]float64, len(b)/2)
	for i := range out {
		out[i] = float64(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32767
	}
	return out
}

func pcmRMS(b []byte) float64 {
	var s float64
	n := len(b) / 2
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(b[2*i:])))
		s += v * v
	}
	return math.Sqrt(s / float64(n))
}

func whiteNoise(samples int, rms float64, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, 2*samples)
	for i := 0; i < samples; i++ {
		v := r.NormFloat64() * rms
		v = math.Max(-32768, math.Min(32767, v))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(v)))
	}
	return b
}

// TestPipelineIdentityReconstructs drives the DSP around the model (48k->16k,
// STFT, iSTFT, 16k->48k, output FIFO) with a pass-through "model": the output
// must be the input delayed by exactly LatencySamples, every chunk returning
// exactly as many bytes as it was given.
func TestPipelineIdentityReconstructs(t *testing.T) {
	p := newPipeline(func([]float32) error { return nil })
	n := 48000
	in := make([]float64, n)
	for i := range in {
		in[i] = 0.3*math.Sin(2*math.Pi*440*float64(i)/48000) + 0.2*math.Sin(2*math.Pi*2500*float64(i)/48000)
	}
	pcm := toPCM(in)
	var out []byte
	for i := 0; i < len(pcm); {
		c := 2048
		if i%3 == 0 {
			c = 1001 // odd byte counts too
		}
		if i+c > len(pcm) {
			c = len(pcm) - i
		}
		o := p.process(pcm[i : i+c])
		if len(o) != c {
			t.Fatalf("chunk of %d bytes returned %d bytes", c, len(o))
		}
		out = append(out, o...)
		i += c
	}
	got := fromPCM(out)
	var maxErr float64
	for i := LatencySamples + 1000; i < len(got); i++ {
		if d := math.Abs(got[i] - in[i-LatencySamples]); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 0.01 {
		t.Fatalf("pass-through pipeline: max error %.4f vs input delayed by %d samples, want <= 0.01", maxErr, LatencySamples)
	}
}

func TestNewMissingModelErrors(t *testing.T) {
	lib := ortLib(t)
	d, err := New(lib, "/nonexistent/denoise.onnx")
	if err == nil {
		d.Close()
		t.Fatal("New with a missing model returned nil error")
	}
}

func TestProcessPreservesLength(t *testing.T) {
	d := newDenoiser(t)
	noise := whiteNoise(100*1024, 1000, 1)
	total := 0
	for i := 0; i < 100; i++ {
		out := d.Process(noise[i*2048 : (i+1)*2048])
		if len(out) != 2048 {
			t.Fatalf("chunk %d: got %d bytes, want 2048", i, len(out))
		}
		total += len(out)
	}
	if total != len(noise) {
		t.Fatalf("total output %d bytes, want %d", total, len(noise))
	}
}

func TestProcessReducesNoise(t *testing.T) {
	d := newDenoiser(t)
	in := whiteNoise(5*48000, 2000, 2)
	var out []byte
	for i := 0; i < len(in); i += 2048 {
		out = append(out, d.Process(in[i:min(i+2048, len(in))])...)
	}
	// Skip the first second: the model's recurrent state is still adapting.
	inRMS, outRMS := pcmRMS(in[2*48000:]), pcmRMS(out[2*48000:])
	t.Logf("white noise RMS %.0f -> %.0f", inRMS, outRMS)
	if outRMS >= 0.5*inRMS {
		t.Fatalf("output RMS %.0f, want < 50%% of input RMS %.0f", outRMS, inRMS)
	}
}

func TestProcessRealtime(t *testing.T) {
	d := newDenoiser(t)
	in := whiteNoise(10*48000, 1000, 3)
	start := time.Now()
	for i := 0; i < len(in); i += 2048 {
		d.Process(in[i:min(i+2048, len(in))])
	}
	el := time.Since(start)
	t.Logf("10 s of audio denoised in %s", el)
	if el >= 3*time.Second {
		t.Fatalf("10 s of audio took %s, want < 3s", el)
	}
}

// After Close, Process passes audio through unchanged (still equal length) so
// a late chunk at shutdown never touches a released session.
func TestProcessAfterClosePassesThrough(t *testing.T) {
	d := newDenoiser(t)
	d.Close()
	in := whiteNoise(1024, 1000, 4)
	out := d.Process(in)
	if string(out) != string(in) {
		t.Fatal("Process after Close did not pass audio through")
	}
}

func TestWrapClosesWithInput(t *testing.T) {
	d := newDenoiser(t)
	in := make(chan []byte, 3)
	in <- make([]byte, 2048)
	in <- make([]byte, 100)
	close(in)
	out := Wrap(in, d)
	var sizes []int
	for c := range out {
		sizes = append(sizes, len(c))
	}
	if len(sizes) != 2 || sizes[0] != 2048 || sizes[1] != 100 {
		t.Fatalf("got chunk sizes %v, want [2048 100]", sizes)
	}
}
