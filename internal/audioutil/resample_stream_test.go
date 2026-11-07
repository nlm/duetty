package audioutil

import (
	"math"
	"testing"
)

func sine(n int, freq, rate, amp float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/rate)
	}
	return out
}

// maxErrAfter compares got[i] with want[i-delay] for i >= skip.
func maxErrAfter(got, want []float64, delay, skip int) float64 {
	var m float64
	for i := skip; i < len(got); i++ {
		j := i - delay
		if j < 0 || j >= len(want) {
			continue
		}
		if d := math.Abs(got[i] - want[j]); d > m {
			m = d
		}
	}
	return m
}

func TestDecimatorChunkingInvariant(t *testing.T) {
	in := sine(48000, 440, 48000, 0.5)
	whole := NewDecimator48to16().Process(in)
	if len(whole) != 16000 {
		t.Fatalf("one-shot output %d samples, want 16000", len(whole))
	}
	d := NewDecimator48to16()
	var pieces []float64
	for i, sizes := 0, []int{1, 2, 5, 767, 768, 1000, 3}; i < len(in); {
		n := sizes[i%len(sizes)]
		if i+n > len(in) {
			n = len(in) - i
		}
		pieces = append(pieces, d.Process(in[i:i+n])...)
		i += n
	}
	if len(pieces) != len(whole) {
		t.Fatalf("chunked output %d samples, want %d", len(pieces), len(whole))
	}
	for i := range whole {
		if pieces[i] != whole[i] {
			t.Fatalf("sample %d: chunked %v != one-shot %v", i, pieces[i], whole[i])
		}
	}
}

func TestDecimatorPassesSpeechBandRejectsAlias(t *testing.T) {
	// 1 kHz stays: output m ~= input(3m - DecimatorDelay48).
	in := sine(48000, 1000, 48000, 0.5)
	out := NewDecimator48to16().Process(in)
	var m float64
	for i := 100; i < len(out); i++ {
		j := 3*i - DecimatorDelay48
		if d := math.Abs(out[i] - in[j]); d > m {
			m = d
		}
	}
	if m > 0.01 {
		t.Fatalf("1 kHz tone: max error %.4f after decimation, want <= 0.01", m)
	}
	// 12 kHz would alias to 4 kHz: must be strongly attenuated.
	alias := NewDecimator48to16().Process(sine(48000, 12000, 48000, 0.5))
	if r := rms(alias[100:]); r > 0.01 {
		t.Fatalf("12 kHz tone leaks with RMS %.4f, want <= 0.01", r)
	}
}

func TestInterpolatorUpsamplesTone(t *testing.T) {
	in := sine(16000, 1000, 16000, 0.5)
	it := NewInterpolator16to48()
	var out []float64
	for i := 0; i < len(in); i += 256 {
		out = append(out, it.Process(in[i:min(i+256, len(in))])...)
	}
	if len(out) != 3*len(in) {
		t.Fatalf("output %d samples, want %d", len(out), 3*len(in))
	}
	ref := sine(48000, 1000, 48000, 0.5)
	if m := maxErrAfter(out, ref, InterpolatorDelay48, 300); m > 0.01 {
		t.Fatalf("max error %.4f vs ideal 48 kHz tone, want <= 0.01", m)
	}
}

func rms(x []float64) float64 {
	var s float64
	for _, v := range x {
		s += v * v
	}
	return math.Sqrt(s / float64(len(x)))
}
