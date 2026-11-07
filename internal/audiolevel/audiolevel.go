// Package audiolevel provides small helpers for working with raw microphone
// PCM levels: RMS computation and a simple adaptive voice-activity detector,
// shared between the run subcommand's voice-activity detector and the
// mic-levels subcommand's calibration tool.
package audiolevel

import (
	"fmt"
	"math"
	"time"

	"github.com/nlm/duetty/internal/audioutil"
)

// RMS computes the root-mean-square amplitude of signed 16-bit
// little-endian PCM samples, as delivered by internal/audioio's MicSource.
func RMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(n))
}

// VAD is a simple adaptive, energy-based voice activity detector over
// signed 16-bit little-endian mono PCM chunks. On construction it spends its
// first warmupMS of audio seeding the ambient noise floor with a fast-moving
// average (see Feed) instead of classifying anything as speech, so it
// self-calibrates to a given room/microphone without needing a separate
// manual calibration step. After warmup it tracks the floor with a slower
// exponential moving average (updated only on confidently-quiet chunks) and
// uses two thresholds -- StartThreshold to confirm an utterance began,
// KeepThreshold (lower) to decide it's still ongoing -- so a soft, gradually
// loudening onset or a brief dip mid-word doesn't reset the onset/end
// debounce timers the way a single hard threshold would.
//
// This is intentionally simple (no ML model, no external dependency) --
// good enough to drive a demo's turn-taking, not a production-grade VAD.
// -vad-sensitivity/-vad-silence-ms/-vad-min-rms/-vad-onset-ms (run
// subcommand flags) still let you retune it further; see the mic-levels
// subcommand for a calibration tool that suggests starting values.
type VAD struct {
	sampleRate  int
	sensitivity float64 // multiplier over the noise floor to count as speech
	minRMS      float64 // absolute floor, for near-silent noise floor estimates
	onsetMS     float64 // sustained loud audio needed to confirm speech start
	silenceMS   float64 // sustained quiet audio needed to confirm speech end
	lowRatio    float64 // KeepThreshold = StartThreshold * lowRatio

	noiseFloor float64
	warmedMS   float64 // ms of warmup audio processed so far, capped at warmupMS
	speaking   bool
	speechAcc  float64 // accumulated ms of loud audio while not yet speaking
	silenceAcc float64 // accumulated ms of quiet audio while speaking
	lastRMS    float64 // RMS of the most recent chunk passed to Feed, for DetectionLabel
	lastVoiced float64 // ms of the most recent chunk classified as speech, for VoicedMS
}

// warmupMS is how long, at startup, Feed spends seeding noiseFloor from raw
// ambient audio (via a fast EMA) before it will ever report started=true.
// This is what lets the VAD self-calibrate to a room without a separate
// manual calibration pass.
const warmupMS = 500

// NewVAD constructs a VAD for PCM sampled at sampleRate Hz. See VAD's doc
// comment for what sensitivity/minRMS/silenceMS/onsetMS/lowRatio control.
func NewVAD(sampleRate int, sensitivity, minRMS, silenceMS, onsetMS, lowRatio float64) *VAD {
	return &VAD{
		sampleRate:  sampleRate,
		sensitivity: sensitivity,
		minRMS:      minRMS,
		onsetMS:     onsetMS,
		silenceMS:   silenceMS,
		lowRatio:    lowRatio,
	}
}

// Feed processes one PCM chunk and reports whether it marked the start or
// end of an utterance (at most one of the two per call).
func (v *VAD) Feed(pcm []byte) (started, ended bool) {
	rms := RMS(pcm)
	v.lastRMS = rms
	// Computed via audioutil.PCMDuration rather than a hand-rolled formula
	// (verified equivalent to sub-microsecond precision -- utterly
	// negligible next to this VAD's own onset/silence debounce, which
	// operates in whole milliseconds) so this codebase has one shared
	// samples-to-duration conversion instead of three independent copies.
	durationMS := float64(audioutil.PCMDuration(pcm, v.sampleRate)) / float64(time.Millisecond)
	v.lastVoiced = 0

	// Warmup: seed the noise floor fast from raw ambient audio, and refuse
	// to classify anything as speech until it's had a chance to settle --
	// otherwise a loud warmup chunk (e.g. a door closing as the robot wakes
	// up) could kick off the slower EMA from a bad floor estimate, or worse,
	// spuriously start an utterance.
	if v.warmedMS < warmupMS {
		v.warmedMS += durationMS
		const warmupAlpha = 0.3
		v.noiseFloor = v.noiseFloor*(1-warmupAlpha) + rms*warmupAlpha
		return false, false
	}

	startThreshold := v.StartThreshold()
	keepThreshold := v.KeepThreshold()
	if rms > startThreshold {
		v.lastVoiced = durationMS
	}

	if !v.speaking {
		if rms > startThreshold {
			v.speechAcc += durationMS
			if v.speechAcc >= v.onsetMS {
				v.speaking = true
				v.speechAcc = 0
				v.silenceAcc = 0
				started = true
			}
			return started, false
		}
		// Ambient-level chunk (rms <= startThreshold): fold it into the noise
		// floor so the detector keeps adapting to a room that gets louder or
		// quieter mid-session, not only during startup warmup. Steady ambient
		// often sits in the keep..start band itself, so skipping the update
		// here would freeze the floor and let it drift only downward.
		const emaAlpha = 0.05
		v.noiseFloor = v.noiseFloor*(1-emaAlpha) + rms*emaAlpha
		// Only a confidently-quiet chunk (below keepThreshold) resets the onset
		// debounce. A chunk in the keep..start hysteresis band instead holds
		// the accumulator -- that band is exactly where a softly-started
		// sentence's leading edge lands, and treating it as silence would clip
		// the first syllable.
		if rms < keepThreshold {
			v.speechAcc = 0
		}
		return started, false
	}

	// Speaking: only a confidently-quiet chunk (below keepThreshold, the
	// lower bar) counts toward ending the utterance -- a chunk in the
	// keep..start band is still plausibly speech (e.g. a trailing-off word),
	// so it resets the silence debounce rather than starting it.
	if rms >= keepThreshold {
		v.silenceAcc = 0
		return false, false
	}
	v.silenceAcc += durationMS
	if v.silenceAcc >= v.silenceMS {
		v.speaking = false
		v.silenceAcc = 0
		return false, true
	}
	return false, false
}

// VoicedMS reports how much of the most recent Feed call's chunk counted as
// speech (implementing vad.SpeechMeter): the whole chunk when it was above
// StartThreshold, else 0 -- the hysteresis band a trailing-off word lands
// in counts for keeping an utterance open, not as speech here, so this
// errs toward reporting less. Always 0 during warmup.
func (v *VAD) VoicedMS() float64 { return v.lastVoiced }

// Speaking reports whether the VAD currently considers itself mid-utterance.
func (v *VAD) Speaking() bool { return v.speaking }

// Reset forces the VAD back to "not speaking," discarding any in-progress
// onset/silence debounce accumulation -- used by a caller (vad.Listener.
// SetMuted) that finalizes an in-progress utterance early instead of
// waiting for Feed to naturally report ended, so a later Feed call doesn't
// still believe it's mid-utterance from before muting. Calibration state
// (noiseFloor, warmedMS, lastRMS) is untouched -- only in-progress
// utterance state resets.
func (v *VAD) Reset() {
	v.speaking = false
	v.speechAcc = 0
	v.silenceAcc = 0
}

// DetectionLabel returns a short diagnostic string describing the most
// recent Feed call's classification, for the -log-state "speech detected"
// line (see voiceDetector in listener.go, package main).
func (v *VAD) DetectionLabel() string {
	return fmt.Sprintf("level=%.0f start=%.0f floor=%.0f", v.lastRMS, v.StartThreshold(), v.NoiseFloor())
}

// NoiseFloor returns the current ambient noise floor estimate (RMS).
func (v *VAD) NoiseFloor() float64 { return v.noiseFloor }

// StartThreshold returns the RMS level a chunk must currently exceed to
// confirm the start of speech: max(NoiseFloor * sensitivity, minRMS).
func (v *VAD) StartThreshold() float64 {
	return math.Max(v.noiseFloor*v.sensitivity, v.minRMS)
}

// KeepThreshold returns the (lower) RMS level below which a chunk counts as
// confidently quiet, either resetting the onset debounce before speech is
// confirmed or counting toward the silence debounce once it's ongoing:
// StartThreshold * lowRatio. The gap between KeepThreshold and
// StartThreshold is a hysteresis band that a soft onset or a mid-word dip
// can land in without flipping the VAD's state.
func (v *VAD) KeepThreshold() float64 {
	return v.StartThreshold() * v.lowRatio
}
