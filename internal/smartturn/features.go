// This file computes the log-mel front end of Pipecat's Smart Turn v3
// end-of-turn model, matching its Python reference (Whisper's feature
// extractor) numerically so the ONNX model sees the inputs it was trained on.
// (Package documentation lives in smartturn.go.)

package smartturn

import (
	"math"
	"sync"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	SampleRate16k = 16000
	WindowSamples = 128000 // 8 s: the model's fixed input window
	NMels         = 80
	NFrames       = 800

	nFFT     = 400
	hop      = 160
	nBins    = nFFT/2 + 1
	melFloor = 1e-10
	varEps   = 1e-7
)

var (
	filtersOnce sync.Once
	filters     [][]float64 // [mel][bin]
	window      []float64
)

// hannPeriodic matches torch.hann_window (periodic), which Whisper uses; a
// symmetric window would shift every spectrum slightly.
func hannPeriodic(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}

func hzToMel(f float64) float64 {
	if f >= 1000 {
		return 15 + math.Log(f/1000)*27/math.Log(6.4)
	}
	return 3 * f / 200
}

func melToHz(m float64) float64 {
	if m >= 15 {
		return 1000 * math.Exp(math.Log(6.4)/27*(m-15))
	}
	return 200 * m / 3
}

// slaneyMelFilters builds triangular filters on the Slaney mel scale with
// Slaney area normalization, as librosa/transformers do.
func slaneyMelFilters(nMels, nFFT, sr int, fmin, fmax float64) [][]float64 {
	bins := nFFT/2 + 1
	mmin, mmax := hzToMel(fmin), hzToMel(fmax)
	pts := make([]float64, nMels+2)
	for i := range pts {
		pts[i] = melToHz(mmin + (mmax-mmin)*float64(i)/float64(nMels+1))
	}
	out := make([][]float64, nMels)
	for m := range out {
		row := make([]float64, bins)
		lo, mid, hi := pts[m], pts[m+1], pts[m+2]
		enorm := 2 / (hi - lo)
		for b := range row {
			f := float64(sr/2) * float64(b) / float64(bins-1)
			down := (f - lo) / (mid - lo)
			up := (hi - f) / (hi - mid)
			row[b] = math.Max(0, math.Min(down, up)) * enorm
		}
		out[m] = row
	}
	return out
}

// reflect returns the index of position i in a reflect-padded signal of
// length n (no repeated edge sample), for i possibly outside [0, n).
func reflect(i, n int) int {
	if i < 0 {
		return -i
	}
	if i >= n {
		return 2*(n-1) - i
	}
	return i
}

// Features returns the model's input_features[0] as row-major [mel][frame]
// (NMels*NFrames values). Input is 16 kHz mono; only the last 8 s are used
// and shorter input is zero-padded at the FRONT, so speech ends at the right
// edge as the model was trained. Normalization runs over the padded buffer
// (padding zeros included), mirroring the reference call path.
func Features(samples16k []float32) []float32 {
	filtersOnce.Do(func() {
		filters = slaneyMelFilters(NMels, nFFT, SampleRate16k, 0, SampleRate16k/2)
		window = hannPeriodic(nFFT)
	})

	x := make([]float64, WindowSamples)
	if len(samples16k) > WindowSamples {
		samples16k = samples16k[len(samples16k)-WindowSamples:]
	}
	off := WindowSamples - len(samples16k)
	for i, v := range samples16k {
		x[off+i] = float64(v)
	}
	var mean float64
	for _, v := range x {
		mean += v
	}
	mean /= WindowSamples
	var variance float64
	for _, v := range x {
		variance += (v - mean) * (v - mean)
	}
	variance /= WindowSamples
	inv := 1 / math.Sqrt(variance+varEps)
	for i := range x {
		x[i] = (x[i] - mean) * inv
	}

	fft := fourier.NewFFT(nFFT)
	frames := WindowSamples/hop + 1 // 801, last one dropped below
	logmel := make([]float64, NMels*frames)
	buf := make([]float64, nFFT)
	coef := make([]complex128, nBins)
	power := make([]float64, nBins)
	for f := 0; f < frames; f++ {
		start := f*hop - nFFT/2
		for i := 0; i < nFFT; i++ {
			buf[i] = x[reflect(start+i, WindowSamples)] * window[i]
		}
		fft.Coefficients(coef, buf)
		for b, c := range coef {
			power[b] = real(c)*real(c) + imag(c)*imag(c)
		}
		for m, row := range filters {
			var s float64
			for b, w := range row {
				s += w * power[b]
			}
			logmel[m*frames+f] = math.Log10(math.Max(melFloor, s))
		}
	}

	gmax := math.Inf(-1)
	for m := 0; m < NMels; m++ {
		for f := 0; f < NFrames; f++ {
			gmax = math.Max(gmax, logmel[m*frames+f])
		}
	}
	out := make([]float32, NMels*NFrames)
	for m := 0; m < NMels; m++ {
		for f := 0; f < NFrames; f++ {
			v := math.Max(logmel[m*frames+f], gmax-8)
			out[m*NFrames+f] = float32((v + 4) / 4)
		}
	}
	return out
}
