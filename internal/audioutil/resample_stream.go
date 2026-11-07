package audioutil

// Streaming (stateful) 48 kHz <-> 16 kHz resamplers for continuous audio such
// as the denoiser's mic path, where ResampleTo16kHz's per-buffer filtering
// would leave a seam at every chunk boundary. Both use the same 63-tap
// Hamming-windowed low-pass as ResampleTo16kHz, applied causally: the output
// is delayed by the filter's group delay (DecimatorDelay48 /
// InterpolatorDelay48, in 48 kHz samples) and the result does not depend on
// how the input is split into calls.

const (
	// DecimatorDelay48 is Decimator's delay in 48 kHz samples: output m
	// approximates input sample 3m - DecimatorDelay48.
	DecimatorDelay48 = (firNumTaps - 1) / 2
	// InterpolatorDelay48 is Interpolator's delay in 48 kHz output samples:
	// output n approximates the input at time n - InterpolatorDelay48.
	InterpolatorDelay48 = (firNumTaps - 1) / 2
)

// fir is a causal FIR filter keeping the last len(coeffs)-1 inputs.
type fir struct {
	coeffs []float64
	buf    []float64 // history (len(coeffs)-1) followed by pending input
}

func newFIR(gain float64) fir {
	c := lowpassFIRCoefficients(NativeSampleRate, firCutoffHz, firNumTaps)
	for i := range c {
		c[i] *= gain
	}
	return fir{coeffs: c, buf: make([]float64, len(c)-1)}
}

// at returns the filter output for buf index p (p >= len(coeffs)-1).
func (f *fir) at(p int) float64 {
	var acc float64
	for k, c := range f.coeffs {
		acc += c * f.buf[p-k]
	}
	return acc
}

// keepHistory drops everything but the last len(coeffs)-1 samples of buf.
func (f *fir) keepHistory() {
	h := len(f.coeffs) - 1
	f.buf = append(f.buf[:0], f.buf[len(f.buf)-h:]...)
}

// Decimator converts a 48 kHz stream to 16 kHz.
type Decimator struct {
	f     fir
	phase int // input samples to skip before the next output (0..2)
}

// NewDecimator48to16 returns a Decimator with zero history.
func NewDecimator48to16() *Decimator {
	return &Decimator{f: newFIR(1)}
}

// Process filters and decimates in, returning one sample per three input
// samples consumed so far (the remainder is carried to the next call).
func (d *Decimator) Process(in []float64) []float64 {
	d.f.buf = append(d.f.buf, in...)
	out := make([]float64, 0, (len(in)+2)/decimationFactor)
	p := len(d.f.coeffs) - 1 + d.phase
	for ; p < len(d.f.buf); p += decimationFactor {
		out = append(out, d.f.at(p))
	}
	d.phase = p - len(d.f.buf)
	d.f.keepHistory()
	return out
}

// Interpolator converts a 16 kHz stream to 48 kHz.
type Interpolator struct {
	f fir
}

// NewInterpolator16to48 returns an Interpolator with zero history.
func NewInterpolator16to48() *Interpolator {
	return &Interpolator{f: newFIR(decimationFactor)}
}

// Process zero-stuffs in by three and low-pass filters it (image rejection),
// returning exactly 3*len(in) samples.
func (it *Interpolator) Process(in []float64) []float64 {
	h := len(it.f.coeffs) - 1
	for _, v := range in {
		it.f.buf = append(it.f.buf, v, 0, 0)
	}
	out := make([]float64, 0, decimationFactor*len(in))
	for p := h; p < len(it.f.buf); p++ {
		out = append(out, it.f.at(p))
	}
	it.f.keepHistory()
	return out
}
