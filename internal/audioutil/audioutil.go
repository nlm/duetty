// Package audioutil holds small, pure-Go audio front-end helpers shared by
// every consumer of the robot microphone's raw PCM stream that needs a lower
// sample rate than the microphone's native one: internal/voicerec (speaker
// embedding) and internal/silerovad (neural voice-activity detection) both
// need the exact same 48kHz-to-16kHz decimation, so it lives here once
// rather than being duplicated (or drifting) between the two packages. It
// knows nothing about PCM's int16-range vs. normalized [-1,1] float
// conventions -- ResampleTo16kHz operates purely on []float64, leaving
// per-consumer scaling to the call site (voicerec keeps the raw int16
// range; silerovad normalizes to [-1,1] before resampling, per Silero's own
// input convention).
package audioutil

import (
	"fmt"
	"math"
	"time"
)

const (
	// TargetSampleRate is the sample rate ResampleTo16kHz always resamples
	// to.
	TargetSampleRate = 16000

	// NativeSampleRate is the only input sample rate ResampleTo16kHz knows
	// how to convert from: the microphone's fixed rate
	// (internal/audioio.SampleRate). The decimation factor below (3) only
	// works because 48000/16000 is exactly 3; a different input rate would
	// need a different (or fractional, interpolating) resampler, which this
	// package doesn't implement since it's never actually needed here.
	NativeSampleRate = 48000

	// decimationFactor is NativeSampleRate/TargetSampleRate -- exact, so
	// resampling is a plain "low-pass then keep every Nth sample" decimation
	// rather than needing polyphase/fractional resampling.
	decimationFactor = NativeSampleRate / TargetSampleRate

	// firCutoffHz is the anti-alias low-pass filter's cutoff frequency,
	// applied before decimating 48kHz to 16kHz. It needs to sit at or below
	// 16kHz's Nyquist frequency (8kHz) with enough transition margin that a
	// finite (non-brickwall) FIR doesn't leave meaningful aliased energy
	// above 8kHz; 7.5kHz gives a 500Hz transition band, a conventional
	// margin for this kind of pre-decimation filter.
	firCutoffHz = 7500.0
	// firNumTaps is the anti-alias FIR's length: odd (so the filter has a
	// well-defined integer-sample center/zero-phase delay) and large enough
	// for a reasonably sharp roll-off at firCutoffHz without being
	// expensive -- 63 taps at 48kHz is a common, unremarkable choice for
	// this kind of pre-decimation low-pass.
	firNumTaps = 63
)

// BytesToInt16LE interprets raw as little-endian 16-bit signed PCM samples
// (the shape internal/audioio's MicSource delivers), one int16 per 2 bytes.
func BytesToInt16LE(raw []byte) []int16 {
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(uint16(raw[i*2]) | uint16(raw[i*2+1])<<8)
	}
	return out
}

// PCMDuration returns how much audio raw represents at sampleRate, assuming
// the same 16-bit little-endian mono convention BytesToInt16LE decodes (2
// bytes/sample) -- the shape every raw PCM buffer in this codebase (mic
// capture, an utterance handed to Embed/Transcribe) already uses. Used by
// internal/voices' speaker-match/enroll duration gates, which need to know
// how much real audio a captured utterance held before trusting a
// speaker-embedding score computed from it (see that package's
// MinMatchDuration/MinEnrollDuration for why).
func PCMDuration(raw []byte, sampleRate int) time.Duration {
	numSamples := len(raw) / 2
	return time.Duration(numSamples) * time.Second / time.Duration(sampleRate)
}

// ResampleTo16kHz downsamples samples from sampleRateIn to TargetSampleRate
// via an anti-alias FIR low-pass (see firCutoffHz/firNumTaps) followed by
// decimation. It only actually implements the one conversion this program
// ever needs -- 48kHz (the robot microphone's fixed rate) down to 16kHz --
// and errors out for any other input rate rather than silently
// mis-resampling; sampleRateIn == TargetSampleRate is accepted as a no-op
// passthrough (useful for tests that want to feed already-16kHz audio
// directly to a downstream stage without exercising the resampler).
// samples can be in any consistent scale (raw int16 range, normalized
// [-1,1], etc.) -- the filter/decimation math is scale-invariant, so
// whatever convention the caller used going in comes back out unchanged.
func ResampleTo16kHz(samples []float64, sampleRateIn int) ([]float64, error) {
	if sampleRateIn == TargetSampleRate {
		out := make([]float64, len(samples))
		copy(out, samples)
		return out, nil
	}
	if sampleRateIn != NativeSampleRate {
		return nil, fmt.Errorf("audioutil: unsupported input sample rate %d (only %d or %d are supported)",
			sampleRateIn, TargetSampleRate, NativeSampleRate)
	}
	coeffs := lowpassFIRCoefficients(float64(sampleRateIn), firCutoffHz, firNumTaps)
	filtered := applyFIR(samples, coeffs)
	return decimate(filtered, decimationFactor), nil
}

// lowpassFIRCoefficients designs a windowed-sinc low-pass FIR filter with
// numTaps taps (must be odd, for a symmetric integer-sample delay),
// Hamming-windowed to control stopband ripple, normalized to unity gain at
// DC (so decimated silence/DC stays silence/DC rather than being scaled).
func lowpassFIRCoefficients(sampleRate, cutoffHz float64, numTaps int) []float64 {
	coeffs := make([]float64, numTaps)
	fc := cutoffHz / sampleRate // normalized cutoff, cycles/sample
	mid := float64(numTaps-1) / 2

	var sum float64
	for i := 0; i < numTaps; i++ {
		n := float64(i) - mid
		var sinc float64
		if n == 0 {
			sinc = 2 * fc
		} else {
			sinc = math.Sin(2*math.Pi*fc*n) / (math.Pi * n)
		}
		// Hamming window.
		window := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(numTaps-1))
		coeffs[i] = sinc * window
		sum += coeffs[i]
	}
	for i := range coeffs {
		coeffs[i] /= sum
	}
	return coeffs
}

// applyFIR convolves x with coeffs, returning a same-length output (the
// standard "same" convolution boundary: coeffs's center tap aligns with
// each output sample, treating x as zero outside its bounds). Since coeffs
// only ever has an odd length here, the center tap is always an exact
// integer index.
func applyFIR(x, coeffs []float64) []float64 {
	half := len(coeffs) / 2
	out := make([]float64, len(x))
	for i := range x {
		var acc float64
		for k, c := range coeffs {
			j := i + k - half
			if j < 0 || j >= len(x) {
				continue
			}
			acc += x[j] * c
		}
		out[i] = acc
	}
	return out
}

// decimate keeps every factor-th sample of x, starting at index 0.
func decimate(x []float64, factor int) []float64 {
	n := len(x) / factor
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = x[i*factor]
	}
	return out
}
