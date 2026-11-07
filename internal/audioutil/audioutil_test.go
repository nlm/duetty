package audioutil

import (
	"math"
	"testing"
	"time"
)

func TestPCMDuration(t *testing.T) {
	cases := []struct {
		name       string
		numBytes   int
		sampleRate int
		want       time.Duration
	}{
		{"one second at 48kHz", 48000 * 2, 48000, time.Second},
		{"one second at 16kHz", 16000 * 2, 16000, time.Second},
		{"half second at 48kHz", 24000 * 2, 48000, 500 * time.Millisecond},
		{"empty", 0, 48000, 0},
		{"odd trailing byte truncates to whole samples", 48000*2 + 1, 48000, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PCMDuration(make([]byte, c.numBytes), c.sampleRate)
			if got != c.want {
				t.Errorf("PCMDuration(%d bytes, %dHz) = %v, want %v", c.numBytes, c.sampleRate, got, c.want)
			}
		})
	}
}

func TestResampleTo16kHzLengthRatio(t *testing.T) {
	const durationSec = 1.0
	n := int(NativeSampleRate * durationSec)
	samples := make([]float64, n)
	for i := range samples {
		samples[i] = math.Sin(2 * math.Pi * 1000 * float64(i) / NativeSampleRate)
	}
	out, err := ResampleTo16kHz(samples, NativeSampleRate)
	if err != nil {
		t.Fatalf("ResampleTo16kHz: %v", err)
	}
	wantLen := n / decimationFactor
	if len(out) != wantLen {
		t.Errorf("resampled length = %d, want %d (input %d samples / decimationFactor %d, i.e. the 48kHz:16kHz 3:1 ratio)", len(out), wantLen, n, decimationFactor)
	}
}

func TestResampleTo16kHzPassthroughAt16kHz(t *testing.T) {
	samples := []float64{1, 2, 3, 4, 5}
	out, err := ResampleTo16kHz(samples, TargetSampleRate)
	if err != nil {
		t.Fatalf("ResampleTo16kHz: %v", err)
	}
	if len(out) != len(samples) {
		t.Fatalf("passthrough length = %d, want %d", len(out), len(samples))
	}
	for i := range samples {
		if out[i] != samples[i] {
			t.Errorf("passthrough[%d] = %v, want %v", i, out[i], samples[i])
		}
	}
}

func TestResampleTo16kHzRejectsUnsupportedRate(t *testing.T) {
	if _, err := ResampleTo16kHz([]float64{1, 2, 3}, 44100); err == nil {
		t.Error("expected an error for an unsupported input sample rate, got nil")
	}
}

func TestLowpassFIRCoefficientsUnityDCGain(t *testing.T) {
	coeffs := lowpassFIRCoefficients(48000, 7500, 63)
	var sum float64
	for _, c := range coeffs {
		sum += c
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("FIR coefficients sum (DC gain) = %v, want 1", sum)
	}
}

func TestBytesToInt16LE(t *testing.T) {
	// Little-endian encoding of int16 values 1, -1, 32767, -32768.
	raw := []byte{
		0x01, 0x00,
		0xFF, 0xFF,
		0xFF, 0x7F,
		0x00, 0x80,
	}
	want := []int16{1, -1, 32767, -32768}
	got := BytesToInt16LE(raw)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}
