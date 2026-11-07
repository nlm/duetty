package smartturn

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

func synthetic() []float32 {
	x := make([]float32, 48000)
	for n := range x {
		v := 0.3 * math.Sin(2*math.Pi*220*float64(n)/16000)
		if n%1600 < 800 {
			v += 0.1 * math.Sin(2*math.Pi*1330*float64(n)/16000)
		}
		x[n] = float32(v)
	}
	return x
}

func TestFeaturesShape(t *testing.T) {
	if got := len(Features(make([]float32, 16000))); got != NMels*NFrames {
		t.Fatalf("len = %d, want %d", got, NMels*NFrames)
	}
}

func TestFeaturesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden_features.f32")
	if err != nil {
		t.Fatal(err)
	}
	got := Features(synthetic())
	if len(raw) != 4*len(got) {
		t.Fatalf("golden has %d bytes, want %d", len(raw), 4*len(got))
	}
	var maxDiff float64
	for i, g := range got {
		w := math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
		if d := math.Abs(float64(g - w)); d > maxDiff {
			maxDiff = d
		}
	}
	t.Logf("max abs diff vs golden: %g", maxDiff)
	if maxDiff > 1e-3 {
		t.Fatalf("max abs diff %g > 1e-3", maxDiff)
	}
}

func TestFeaturesUsesLastWindow(t *testing.T) {
	b := synthetic()
	long := make([]float32, 0, 20*SampleRate16k)
	for i := 0; len(long) < 20*SampleRate16k-WindowSamples; i++ {
		long = append(long, float32(math.Sin(float64(i)*0.37)))
	}
	long = append(long, make([]float32, WindowSamples-len(b))...)
	long = append(long, b...)
	want := Features(long[len(long)-WindowSamples:])
	got := Features(long)
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d", i)
		}
	}
}

func TestFeaturesFrontPadding(t *testing.T) {
	tone := make([]float32, SampleRate16k)
	for n := range tone {
		tone[n] = float32(0.3 * math.Sin(2*math.Pi*440*float64(n)/16000))
	}
	f := Features(tone)
	same := func(a, b int) bool {
		for m := 0; m < NMels; m++ {
			if f[m*NFrames+a] != f[m*NFrames+b] {
				return false
			}
		}
		return true
	}
	for fr := 1; fr < 690; fr++ {
		if !same(0, fr) {
			t.Fatalf("frame %d differs from frame 0 in the padded region", fr)
		}
	}
	if same(0, NFrames-5) {
		t.Fatal("tone region equals silence region")
	}
}
