// Package silerovad is the in-process ONNX Silero VAD (voice-activity
// detector): a small neural network that scores each 32ms window of 16kHz
// audio with a speech probability, run in-process via
// github.com/yalue/onnxruntime_go -- an opt-in alternative to the default
// energy-based detector in internal/audiolevel (see -vad-engine in the
// parent module's run.go). Unlike the energy VAD, it needs no per-room
// noise-floor calibration, at the cost of needing a model file and
// libonnxruntime.so.
//
// onnxruntime_go loads the native libonnxruntime.so at runtime via dlopen,
// not at link time, so this package -- and every package importing it --
// builds and vets cleanly even on a machine with neither the shared library
// nor any model file present. Nothing in this package touches ONNX until
// NewVAD is called, which only happens when -vad-engine silero is
// explicitly selected.
package silerovad

import (
	"fmt"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/nlm/duetty/internal/audioutil"
	"github.com/nlm/duetty/internal/onnxenv"
	"github.com/nlm/duetty/internal/stderrguard"
)

const (
	// windowSamples is Silero v5's fixed analysis hop: 512 samples at
	// 16kHz (32ms). Feed buffers incoming audio and runs the model exactly
	// once per full window, carrying any <512-sample remainder over to the
	// next call.
	windowSamples = 512
	// contextSamples is the 64-sample tail of the *previous* window that
	// Silero v5 requires prepended to each window before inference (so the
	// model actually sees contextSamples+windowSamples = 576 samples). This
	// mirrors the reference Silero wrapper, which keeps a rolling 64-sample
	// context; omitting it doesn't error but silently collapses the model's
	// speech probabilities (verified: real speech peaks ~1.0 with the context,
	// only ~0.17 without it). The context is zero for the very first window.
	contextSamples = 64
	// modelInputSamples is the actual per-inference input length: the
	// prepended context plus one hop.
	modelInputSamples = contextSamples + windowSamples
	// windowMS is windowSamples's duration in milliseconds -- how much
	// endpointing debounce time (see feedProb) one window's worth of
	// evidence is worth.
	windowMS = float64(windowSamples) / float64(audioutil.TargetSampleRate) * 1000

	// stateLayers/stateBatch/stateHidden describe Silero v5's fixed LSTM
	// state shape, [2, 1, 128]: 2 (stacked recurrent layers), batch size 1,
	// 128 hidden units. stateSize is the flattened element count.
	stateLayers = 2
	stateBatch  = 1
	stateHidden = 128
	stateSize   = stateLayers * stateBatch * stateHidden

	// silenceHysteresis is how far below threshold the speech probability
	// must drop before it counts toward ending an utterance, matching
	// Silero's own reference VADIterator (upstream hardcodes the same 0.15
	// gap between its enter and exit thresholds). Without this gap, a
	// probability oscillating right at threshold would flicker started/
	// ended every other window.
	silenceHysteresis = 0.15
)

// VAD wraps a single Silero VAD ONNX session plus its continuously-threaded
// LSTM state and endpointing state machine. Build one with NewVAD and Close
// it when done. Like facerec.Recognizer/voicerec.Recognizer, it is not safe
// for concurrent use -- the parent program only ever calls Feed from the
// single listener goroutine (see listener.go).
type VAD struct {
	session *ort.DynamicAdvancedSession

	audioName string
	stateName string
	srName    string
	srScalar  bool // true if the model's sr input is a true 0-rank scalar rather than a rank-1 [1] tensor

	sampleRateIn int // input PCM sample rate Feed expects (see audioutil.ResampleTo16kHz)

	threshold float64
	silenceMS float64
	onsetMS   float64

	state   []float32 // LSTM state, threaded through every window, shape [2,1,128] flattened
	context []float32 // last contextSamples of the previous window, prepended to the next (Silero v5 requirement)
	buf     []float32 // resampled samples buffered until a full window (512 samples) is available

	triggered  bool
	speechAcc  float64 // accumulated ms of high-probability audio while not yet triggered
	silenceAcc float64 // accumulated ms of low-probability audio while triggered
	lastProb   float64
	lastVoiced float64 // ms of the most recent Feed call's windows at or above threshold, for VoicedMS
}

// NewVAD loads the Silero VAD ONNX model at modelPath and builds a
// ready-to-use VAD. ortLibPath selects a specific libonnxruntime.so (""
// relies on onnxruntime_go's default search path). provider selects the
// onnxruntime execution provider (see onnxenv.NewSessionOptions -- "" and
// "cpu" are equivalent and the default). threshold is the speech
// probability an audio window must reach to count as speech (the exit
// threshold is threshold-0.15, see silenceHysteresis); silenceMS/onsetMS
// mirror the energy VAD's -vad-end-silence-ms/-vad-onset-ms debounce windows.
// sampleRateIn is the input PCM sample rate Feed will be called with (e.g.
// internal/audioio.SampleRate) -- audioutil.ResampleTo16kHz only actually
// supports 16000 or 48000.
//
// This package targets Silero v5's documented I/O signature specifically:
// three inputs (a [1,576] float32 audio window -- a 64-sample context plus a
// 512-sample hop, see contextSamples -- a [2,1,128] float32 LSTM
// state, and an int64 sample-rate scalar fixed at 16000) and two outputs (a
// [*,1] float32 speech probability and the [2,1,128] float32 updated
// state). Rather than hardcoding tensor *names* (export tools don't
// guarantee stable naming, and Silero's own published exports have varied
// across versions), NewVAD reads the model's own input/output metadata
// (ort.GetInputOutputInfo) and maps each one to its role by shape/rank: the
// only int64 input is the sample-rate scalar, the only rank-2 float input
// is the audio window, and the only rank-3 float input is the state
// (equivalently for the two outputs, by rank). A model that doesn't fit
// that shape -- e.g. Silero v4's different (h/c-split) state signature -- is
// rejected up front with a clear error rather than silently mismatched.
func NewVAD(ortLibPath, modelPath, provider string, threshold, silenceMS, onsetMS float64, sampleRateIn int) (*VAD, error) {
	if err := onnxenv.Ensure(ortLibPath); err != nil {
		return nil, fmt.Errorf("initialize onnxruntime environment: %w", err)
	}

	ins, outs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("read silero VAD model %q metadata: %w", modelPath, err)
	}
	audioName, stateName, srName, srScalar, err := classifyInputs(ins)
	if err != nil {
		return nil, fmt.Errorf("silero VAD model %q: %w", modelPath, err)
	}
	probName, stateOutName, err := classifyOutputs(outs)
	if err != nil {
		return nil, fmt.Errorf("silero VAD model %q: %w", modelPath, err)
	}

	// provider ("" or "cpu" -> no-op, "coreml" -> appends onnxruntime's
	// CoreML execution provider) is resolved first via the shared
	// internal/onnxenv helper, then explicitly bound to 1 intra/inter-op
	// thread regardless of provider, rather than left at the nil-options
	// default (which resolves to "let onnxruntime decide," typically the
	// number of CPU cores): this model runs continuously at ~30
	// inferences/second for the entire time the mic is unmuted (once per
	// 512-sample/32ms window fed via Feed), and its graph is tiny (a
	// single-cell LSTM-style RNN step, [1,576] audio in, [2,1,128] state)
	// -- nowhere near large enough to benefit from intra/inter-op
	// parallelism, but large enough that spinning up and coordinating a
	// multi-core thread pool ~30 times a second dominates the actual
	// compute cost. Left at the default, this alone was observed pinning
	// a full CPU core for as long as the mic stayed unmuted, dropping to
	// near zero the instant it muted (Feed is never called while muted --
	// see vad.Listener.Run) -- a clean signal the cost was this
	// per-inference threading overhead, not anything inherent to the
	// audio processing itself. The same bound is kept even with CoreML
	// selected: CoreML only ever handles part of the graph (falling back
	// to CPU for anything it doesn't support), and this model is small
	// enough that a multi-thread CPU fallback path gains nothing here
	// either.
	sessionOpts, err := onnxenv.NewSessionOptions(provider)
	if err != nil {
		return nil, fmt.Errorf("create silero VAD session options: %w", err)
	}
	defer sessionOpts.Destroy()
	if err := sessionOpts.SetIntraOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("set silero VAD intra-op thread count: %w", err)
	}
	if err := sessionOpts.SetInterOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("set silero VAD inter-op thread count: %w", err)
	}

	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{audioName, stateName, srName},
		[]string{probName, stateOutName}, sessionOpts)
	if err != nil {
		return nil, fmt.Errorf("load silero VAD model %q: %w", modelPath, err)
	}

	return &VAD{
		session:      session,
		audioName:    audioName,
		stateName:    stateName,
		srName:       srName,
		srScalar:     srScalar,
		sampleRateIn: sampleRateIn,
		threshold:    threshold,
		silenceMS:    silenceMS,
		onsetMS:      onsetMS,
		state:        make([]float32, stateSize),
		context:      make([]float32, contextSamples),
	}, nil
}

// classifyInputs maps NewVAD's 3 expected inputs to their roles by shape/
// rank rather than name: the int64 input is the sample-rate scalar
// (srScalar reports whether it's a true 0-rank scalar or a rank-1 [1]
// tensor -- onnxruntime_go represents these as distinct Go types, see
// runWindow), the rank-2 float input is the audio window, and the rank-3
// float input is the LSTM state. Errors if there isn't exactly 3 inputs or
// they don't sort into exactly one of each role.
func classifyInputs(ins []ort.InputOutputInfo) (audioName, stateName, srName string, srScalar bool, err error) {
	if len(ins) != 3 {
		return "", "", "", false, fmt.Errorf("expected 3 inputs (Silero v5's audio/state/sr signature), got %d", len(ins))
	}
	var haveAudio, haveState, haveSR bool
	for _, in := range ins {
		if in.DataType == ort.TensorElementDataTypeInt64 {
			if haveSR {
				return "", "", "", false, fmt.Errorf("two int64 inputs found (expected exactly one sample-rate scalar)")
			}
			haveSR = true
			srName = in.Name
			srScalar = len(in.Dimensions) == 0
			continue
		}
		switch len(in.Dimensions) {
		case 2:
			if haveAudio {
				return "", "", "", false, fmt.Errorf("two rank-2 float inputs found (expected exactly one [1,512] audio window)")
			}
			haveAudio = true
			audioName = in.Name
		case 3:
			if haveState {
				return "", "", "", false, fmt.Errorf("two rank-3 float inputs found (expected exactly one [2,1,128] state)")
			}
			haveState = true
			stateName = in.Name
		default:
			return "", "", "", false, fmt.Errorf("unexpected input %q: rank %d, type %s (want a rank-2 audio window, a rank-3 state, or an int64 sample-rate scalar)",
				in.Name, len(in.Dimensions), in.DataType)
		}
	}
	if !haveAudio || !haveState || !haveSR {
		return "", "", "", false, fmt.Errorf("couldn't identify all 3 inputs by shape (audio=%v state=%v sr=%v) -- not a Silero v5-shaped model?", haveAudio, haveState, haveSR)
	}
	return audioName, stateName, srName, srScalar, nil
}

// classifyOutputs maps NewVAD's 2 expected outputs to their roles by rank:
// the rank-3 output is the updated [2,1,128] LSTM state, the other
// (expected rank <=2, [*,1]) is the speech probability. Errors if there
// isn't exactly 2 outputs or they don't sort into exactly one of each role.
func classifyOutputs(outs []ort.InputOutputInfo) (probName, stateOutName string, err error) {
	if len(outs) != 2 {
		return "", "", fmt.Errorf("expected 2 outputs (speech probability + updated state), got %d", len(outs))
	}
	var haveProb, haveState bool
	for _, o := range outs {
		if len(o.Dimensions) == 3 {
			if haveState {
				return "", "", fmt.Errorf("two rank-3 outputs found (expected exactly one [2,1,128] updated state)")
			}
			haveState = true
			stateOutName = o.Name
			continue
		}
		if haveProb {
			return "", "", fmt.Errorf("two non-rank-3 outputs found (expected exactly one speech-probability output)")
		}
		haveProb = true
		probName = o.Name
	}
	if !haveProb || !haveState {
		return "", "", fmt.Errorf("couldn't identify both outputs by shape (prob=%v state=%v) -- not a Silero v5-shaped model?", haveProb, haveState)
	}
	return probName, stateOutName, nil
}

// Close releases the ONNX session. The VAD must not be used afterward.
func (v *VAD) Close() {
	if v.session != nil {
		v.session.Destroy()
	}
}

// Feed processes one PCM chunk (raw signed 16-bit little-endian mono
// samples at v.sampleRateIn, as captured by internal/audioio's MicSource)
// and reports whether any window it fully consumed confirmed the start or
// end of an utterance. It normalizes to float32 [-1,1] (Silero's own input
// convention), resamples to 16kHz (audioutil.ResampleTo16kHz), and appends
// to an internal buffer; each time >=512 samples are buffered it runs one
// model window (threading the LSTM state through, see runWindow) and feeds
// the resulting probability to feedProb, keeping any <512-sample remainder
// for the next call. A chunk can span multiple windows (or none, if too
// short) -- started/ended are aggregated across all windows this call
// consumed, so a caller polling once per chunk never misses a boundary that
// fell mid-chunk.
func (v *VAD) Feed(pcm []byte) (started, ended bool) {
	v.lastVoiced = 0
	if len(pcm) == 0 {
		return false, false
	}
	resampled, err := v.resample16k(pcm)
	if err != nil {
		// sampleRateIn is fixed at NewVAD construction time, so a mismatch
		// here would be a programming error, not a transient one -- nothing
		// useful to do per-chunk except decline to classify this audio
		// (Feed's interface has no error return).
		return false, false
	}
	v.buf = append(v.buf, resampled...)

	for len(v.buf) >= windowSamples {
		window := v.buf[:windowSamples]
		v.buf = v.buf[windowSamples:]

		prob, err := v.runWindow(window)
		if err != nil {
			// A mid-stream inference failure on an already-successfully-
			// loaded session is unexpected; skip this window's endpointing
			// evidence rather than guessing or corrupting the debounce
			// state.
			continue
		}
		if prob >= v.threshold {
			v.lastVoiced += windowMS
		}
		s, e := v.feedProb(prob, windowMS)
		started = started || s
		ended = ended || e
	}

	// Bound v.buf's backing array to just the leftover (<512 sample)
	// remainder rather than letting repeated front-slicing hold the whole
	// session's audio alive via one ever-growing array.
	if len(v.buf) == 0 {
		v.buf = nil
	} else {
		v.buf = append([]float32(nil), v.buf...)
	}
	return started, ended
}

// resample16k converts s16le PCM at v.sampleRateIn to normalized float32
// [-1,1] samples at 16kHz.
func (v *VAD) resample16k(pcm []byte) ([]float32, error) {
	int16s := audioutil.BytesToInt16LE(pcm)
	samples := make([]float64, len(int16s))
	for i, s := range int16s {
		samples[i] = float64(s) / 32768.0
	}
	resampled, err := audioutil.ResampleTo16kHz(samples, v.sampleRateIn)
	if err != nil {
		return nil, err
	}
	out := make([]float32, len(resampled))
	for i, s := range resampled {
		out[i] = float32(s)
	}
	return out, nil
}

// Probs returns the speech probability of every complete 512-sample (32ms @
// 16kHz) window of pcm (s16le mono at the VAD's input sample rate), computed
// on a fresh model state (LSTM state, context and buffered samples are reset
// first). Any trailing partial window is dropped. The VAD is reset again
// afterwards so it can be reused for the next utterance; it does not drive
// the endpointing state machine. Like the rest of VAD, Probs is not safe for
// concurrent use.
func (v *VAD) Probs(pcm48k []byte) ([]float32, error) {
	v.Reset()
	defer v.Reset()
	samples, err := v.resample16k(pcm48k)
	if err != nil {
		return nil, fmt.Errorf("resample: %w", err)
	}
	probs := make([]float32, 0, len(samples)/windowSamples)
	for off := 0; off+windowSamples <= len(samples); off += windowSamples {
		p, err := v.runWindow(samples[off : off+windowSamples])
		if err != nil {
			return nil, err
		}
		probs = append(probs, float32(p))
	}
	return probs, nil
}

// runWindow runs the model once on one windowSamples-sample hop, prepending
// the previous window's 64-sample context (see contextSamples) so the model
// input is modelInputSamples long, and threading v.state through as both the
// input state and (on success) its replacement. It returns the speech
// probability and rolls the context forward for the next call.
func (v *VAD) runWindow(window []float32) (float64, error) {
	// Silero v5 expects the previous window's 64-sample tail prepended, so the
	// model sees contextSamples+windowSamples samples (see contextSamples).
	input := make([]float32, modelInputSamples)
	copy(input, v.context)
	copy(input[contextSamples:], window)

	audioTensor, err := ort.NewTensor(ort.NewShape(1, modelInputSamples), input)
	if err != nil {
		return 0, fmt.Errorf("build audio input tensor: %w", err)
	}
	defer audioTensor.Destroy()

	stateTensor, err := ort.NewTensor(ort.NewShape(stateLayers, stateBatch, stateHidden), v.state)
	if err != nil {
		return 0, fmt.Errorf("build state input tensor: %w", err)
	}
	defer stateTensor.Destroy()

	var srValue ort.Value
	if v.srScalar {
		sc, err := ort.NewScalar[int64](int64(audioutil.TargetSampleRate))
		if err != nil {
			return 0, fmt.Errorf("build sr scalar: %w", err)
		}
		defer sc.Destroy()
		srValue = sc
	} else {
		t, err := ort.NewTensor(ort.NewShape(1), []int64{int64(audioutil.TargetSampleRate)})
		if err != nil {
			return 0, fmt.Errorf("build sr tensor: %w", err)
		}
		defer t.Destroy()
		srValue = t
	}

	outputs := make([]ort.Value, 2)
	var runErr error
	stderrguard.Suppress(func() {
		runErr = v.session.Run([]ort.Value{audioTensor, stateTensor, srValue}, outputs)
	})
	if runErr != nil {
		return 0, fmt.Errorf("run silero VAD model: %w", runErr)
	}
	defer func() {
		for _, o := range outputs {
			if o != nil {
				o.Destroy()
			}
		}
	}()

	probOut, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return 0, fmt.Errorf("probability output has unexpected type %T (want a float32 tensor)", outputs[0])
	}
	probData := probOut.GetData()
	if len(probData) == 0 {
		return 0, fmt.Errorf("probability output is empty")
	}

	stateOut, ok := outputs[1].(*ort.Tensor[float32])
	if !ok {
		return 0, fmt.Errorf("state output has unexpected type %T (want a float32 tensor)", outputs[1])
	}
	stateData := stateOut.GetData()
	if len(stateData) != stateSize {
		return 0, fmt.Errorf("state output has %d values, want %d", len(stateData), stateSize)
	}
	copy(v.state, stateData)
	// Roll the context forward: this window's last contextSamples become the
	// next window's prepended context.
	copy(v.context, window[windowSamples-contextSamples:])

	return float64(probData[0]), nil
}

// feedProb drives the endpointing state machine on one window's speech
// probability p, mirroring Silero's own reference VADIterator: while not
// triggered, p >= threshold accumulates speech time (speechAcc) toward
// confirming the start of an utterance after onsetMS, and any p < threshold
// resets that accumulator (a probability that never even reaches
// threshold is not a "soft onset" worth debouncing, unlike the energy VAD's
// hysteresis band). Once triggered, p < threshold-silenceHysteresis
// accumulates silence time (silenceAcc) toward confirming the end after
// silenceMS, p >= threshold resets it outright, and a probability in
// between (the hysteresis band) holds the accumulator unchanged -- so a
// brief mid-word dip that doesn't fully cross the lower exit threshold, or
// does but not for long enough to reach silenceMS, never ends the
// utterance. windowMS is a parameter (rather than reading the package
// constant directly) purely so this method is unit-testable with synthetic
// probability sequences and made-up timings, without a model (see
// silerovad_test.go).
func (v *VAD) feedProb(p, windowMS float64) (started, ended bool) {
	v.lastProb = p
	exitThreshold := v.threshold - silenceHysteresis

	if !v.triggered {
		if p >= v.threshold {
			v.speechAcc += windowMS
			if v.speechAcc >= v.onsetMS {
				v.triggered = true
				v.speechAcc = 0
				v.silenceAcc = 0
				started = true
			}
		} else {
			v.speechAcc = 0
		}
		return started, false
	}

	switch {
	case p < exitThreshold:
		v.silenceAcc += windowMS
		if v.silenceAcc >= v.silenceMS {
			v.triggered = false
			v.silenceAcc = 0
			v.speechAcc = 0
			ended = true
		}
	case p >= v.threshold:
		v.silenceAcc = 0
	}
	// p in [exitThreshold, threshold): hysteresis band, hold silenceAcc.
	return false, ended
}

// Speaking reports whether the VAD currently considers itself mid-utterance.
func (v *VAD) Speaking() bool { return v.triggered }

// Reset forces the VAD back to "not speaking," discarding any in-progress
// onset/silence debounce accumulation -- used by a caller (vad.Listener.
// SetMuted) that finalizes an in-progress utterance early instead of
// waiting for Feed to naturally report ended, so a later Feed call doesn't
// still believe it's mid-utterance from before muting.
//
// Also restores buf/context/state to the same zero values NewVAD
// constructs them with -- a review caught that leaving them as-is still
// let pre-mute audio leak into post-mute processing despite the
// triggered/debounce reset above: buf holds up to contextSamples-1 raw
// resampled samples not yet fed through the model at all, which would
// otherwise get concatenated directly with the first post-unmute samples
// into one window, splicing raw PCM across the muted gap; context and
// state are Silero's own recurrent inputs (the previous window's tail
// samples, and the LSTM hidden state), which would otherwise let the
// model's very next post-unmute inference be influenced by pre-mute audio
// content -- a subtler instance of the same leak. Feed isn't called at
// all while muted (see Listener.Run), so nothing overwrites these between
// this Reset and the next real Feed call once unmuted.
func (v *VAD) Reset() {
	v.triggered = false
	v.speechAcc = 0
	v.silenceAcc = 0
	v.buf = nil
	v.context = make([]float32, contextSamples)
	v.state = make([]float32, stateSize)
}

// VoicedMS reports how much of the most recent Feed call's audio counted as
// speech (implementing vad.SpeechMeter): windowMS for every window it
// completed whose probability reached threshold -- the onset bar, not the
// lower exit one, so this errs toward reporting less.
func (v *VAD) VoicedMS() float64 { return v.lastVoiced }

// DetectionLabel returns a short diagnostic string describing the most
// recent window's speech probability, for the -log-state "speech detected"
// line (see voiceDetector in the parent module's listener.go).
func (v *VAD) DetectionLabel() string {
	return fmt.Sprintf("prob=%.2f threshold=%.2f", v.lastProb, v.threshold)
}
