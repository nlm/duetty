package silerovad

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// newTestVAD builds a VAD with only the endpointing fields populated --
// exactly what feedProb touches -- so the state machine is unit-testable
// without an ONNX session or model file (session/state/buf are left zero
// and never referenced by feedProb).
func newTestVAD(threshold, silenceMS, onsetMS float64) *VAD {
	return &VAD{threshold: threshold, silenceMS: silenceMS, onsetMS: onsetMS}
}

// TestFeedProbConfirmsStartAfterOnsetMS drives feedProb with a run of
// above-threshold probabilities: it must not report started until the
// accumulated windows reach onsetMS, and must report it (once) on the
// window that reaches it.
func TestFeedProbConfirmsStartAfterOnsetMS(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	const windowMS = 25.0 // 4 windows exactly reach onsetMS=100

	for i := 0; i < 3; i++ {
		started, ended := v.feedProb(0.9, windowMS)
		if started || ended {
			t.Fatalf("window %d: started=%v ended=%v, want false,false (only %vms of %vms accumulated)",
				i, started, ended, float64(i+1)*windowMS, v.onsetMS)
		}
		if v.Speaking() {
			t.Fatalf("window %d: Speaking() = true before onset confirmed", i)
		}
	}
	started, ended := v.feedProb(0.9, windowMS)
	if !started || ended {
		t.Fatalf("4th window: started=%v ended=%v, want true,false", started, ended)
	}
	if !v.Speaking() {
		t.Error("Speaking() = false immediately after start confirmed")
	}
}

// TestFeedProbBelowThresholdResetsSpeechAccumulator confirms a probability
// dropping back below threshold before onsetMS is reached resets the onset
// debounce, per feedProb's documented semantics ("p<threshold while not
// triggered resets the speech accumulator") -- so it takes another full
// onsetMS of sustained high probability to confirm start, not just the
// leftover.
func TestFeedProbBelowThresholdResetsSpeechAccumulator(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	const windowMS = 25.0

	v.feedProb(0.9, windowMS)
	v.feedProb(0.9, windowMS) // 50ms accumulated
	if started, _ := v.feedProb(0.2, windowMS); started {
		t.Fatal("dip below threshold reported started")
	}
	// Only 3 more high windows (75ms) -- if the accumulator wasn't reset,
	// this would already exceed onsetMS (50+75=125 >= 100); since it must
	// reset, it should take a 4th to confirm.
	for i := 0; i < 3; i++ {
		if started, _ := v.feedProb(0.9, windowMS); started {
			t.Fatalf("started confirmed too early on post-dip window %d (accumulator wasn't reset)", i)
		}
	}
	if started, _ := v.feedProb(0.9, windowMS); !started {
		t.Error("started never confirmed after a full onsetMS run following the dip")
	}
}

// TestFeedProbConfirmsEndAfterSilenceMS confirms an utterance that's
// already started, then reports ended once a run of below-(threshold-0.15)
// probabilities accumulates to silenceMS.
func TestFeedProbConfirmsEndAfterSilenceMS(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	const windowMS = 25.0

	for i := 0; i < 4; i++ {
		v.feedProb(0.9, windowMS)
	}
	if !v.Speaking() {
		t.Fatal("setup: expected Speaking() = true after onset run")
	}

	for i := 0; i < 3; i++ {
		started, ended := v.feedProb(0.05, windowMS) // well below threshold-0.15=0.35
		if started || ended {
			t.Fatalf("silence window %d: started=%v ended=%v, want false,false", i, started, ended)
		}
		if !v.Speaking() {
			t.Fatalf("silence window %d: Speaking() = false before end confirmed", i)
		}
	}
	started, ended := v.feedProb(0.05, windowMS)
	if started || !ended {
		t.Fatalf("4th silence window: started=%v ended=%v, want false,true", started, ended)
	}
	if v.Speaking() {
		t.Error("Speaking() = true immediately after end confirmed")
	}
}

// TestResetClearsTriggeredState checks Reset's documented contract: an
// in-progress utterance's triggered/debounce state is cleared, so a
// subsequent feedProb call requires fresh onset debounce rather than
// still believing it's mid-utterance -- used by vad.Listener.SetMuted when
// it finalizes an in-progress utterance early instead of waiting for a
// natural "ended".
func TestResetClearsTriggeredState(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	const windowMS = 25.0

	for i := 0; i < 4; i++ {
		v.feedProb(0.9, windowMS)
	}
	if !v.Speaking() {
		t.Fatal("setup: expected Speaking() = true after onset run")
	}

	v.Reset()

	if v.Speaking() {
		t.Error("Speaking() = true after Reset, want false")
	}
	// A fresh sub-onset-length run of high probabilities must not
	// immediately report started -- Reset must clear the onset
	// accumulator too, not just the triggered flag.
	for i := 0; i < 3; i++ {
		if started, _ := v.feedProb(0.9, windowMS); started {
			t.Fatalf("started confirmed too early on post-reset window %d (onset accumulator wasn't cleared by Reset)", i)
		}
	}
}

// TestResetClearsBufContextAndState is the regression test for a real gap
// a review caught: clearing only triggered/speechAcc/silenceAcc still let
// pre-mute audio leak into post-mute processing via buf (raw resampled
// samples not yet fed through the model), context (the previous window's
// tail samples, a Silero v5 model input), and state (the LSTM hidden
// state) -- all three must reset to the same zero values NewVAD
// constructs them with, not just the plain triggered/debounce fields.
func TestResetClearsBufContextAndState(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	v.buf = []float32{1, 2, 3}
	v.context = []float32{9, 9, 9, 9}
	v.state = []float32{5, 5, 5}

	v.Reset()

	if len(v.buf) != 0 {
		t.Errorf("buf = %v after Reset, want empty (no leftover pre-mute raw samples spliced into the next window)", v.buf)
	}
	if len(v.context) != contextSamples {
		t.Fatalf("len(context) = %d after Reset, want %d", len(v.context), contextSamples)
	}
	for i, c := range v.context {
		if c != 0 {
			t.Errorf("context[%d] = %v after Reset, want 0 (no pre-mute audio leaking into the model's recurrent input)", i, c)
		}
	}
	if len(v.state) != stateSize {
		t.Fatalf("len(state) = %d after Reset, want %d", len(v.state), stateSize)
	}
	for i, s := range v.state {
		if s != 0 {
			t.Errorf("state[%d] = %v after Reset, want 0 (LSTM hidden state must not carry pre-mute context forward)", i, s)
		}
	}
}

// TestFeedProbBriefDipDoesNotEnd confirms a dip below the exit threshold
// that's shorter than silenceMS, followed by a probability back at or above
// threshold, resets the silence debounce entirely (per feedProb's
// documented "p>=threshold while triggered resets the silence accumulator")
// rather than ending the utterance -- e.g. a trailing-off word or brief
// pause mid-sentence shouldn't cut a turn short.
func TestFeedProbBriefDipDoesNotEnd(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	const windowMS = 25.0

	for i := 0; i < 4; i++ {
		v.feedProb(0.9, windowMS)
	}
	if !v.Speaking() {
		t.Fatal("setup: expected Speaking() = true after onset run")
	}

	// A dip well short of silenceMS (50ms of 100ms needed).
	v.feedProb(0.05, windowMS)
	if started, ended := v.feedProb(0.05, windowMS); started || ended {
		t.Fatalf("short dip: started=%v ended=%v, want false,false", started, ended)
	}

	// Probability recovers above threshold -- should reset the silence
	// accumulator outright, not just pause it.
	if started, ended := v.feedProb(0.9, windowMS); started || ended {
		t.Fatalf("recovery window: started=%v ended=%v, want false,false (already triggered)", started, ended)
	}
	if !v.Speaking() {
		t.Fatal("Speaking() = false after recovering above threshold -- dip incorrectly ended the utterance")
	}

	// Another run of silence shorter than silenceMS on its own must not end
	// it either -- if the earlier dip's accumulated time had leaked through
	// instead of being reset, 2x50ms would exceed silenceMS=100 and this
	// would incorrectly end here.
	for i := 0; i < 3; i++ {
		if started, ended := v.feedProb(0.05, windowMS); started || ended {
			t.Fatalf("post-recovery silence window %d: started=%v ended=%v, want false,false", i, started, ended)
		}
	}
	if !v.Speaking() {
		t.Error("Speaking() = false -- the earlier dip's silence time leaked through the reset")
	}
}

// TestFeedProbHysteresisBandHoldsSilenceAccumulator confirms a probability
// in the hysteresis band (threshold-0.15 <= p < threshold) while triggered
// neither accumulates toward ending nor resets what's already accumulated
// -- it's treated as "still plausibly speech" the same way the energy VAD's
// KeepThreshold band is.
func TestFeedProbHysteresisBandHoldsSilenceAccumulator(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	const windowMS = 25.0

	for i := 0; i < 4; i++ {
		v.feedProb(0.9, windowMS)
	}
	// Accumulate some silence time.
	v.feedProb(0.05, windowMS)
	v.feedProb(0.05, windowMS) // 50ms accumulated

	// A hysteresis-band probability (0.35 <= p < 0.5) should hold, not reset.
	v.feedProb(0.4, windowMS)

	// Two more genuine silence windows should now be enough to reach the
	// 100ms silenceMS (50 already + 50 more), confirming the band value
	// didn't reset the accumulator.
	v.feedProb(0.05, windowMS)
	if _, ended := v.feedProb(0.05, windowMS); !ended {
		t.Error("expected ended=true once accumulated silence (excluding the held hysteresis-band window) reaches silenceMS")
	}
}

func TestDetectionLabel(t *testing.T) {
	v := newTestVAD(0.5, 100, 100)
	v.feedProb(0.73, 25)
	label := v.DetectionLabel()
	want := "prob=0.73 threshold=0.50"
	if label != want {
		t.Errorf("DetectionLabel() = %q, want %q", label, want)
	}
}

// requireTestModel skips the test unless SILEROVAD_TEST_MODEL is set,
// pointing it at a real silero_vad.onnx file (and, optionally, a
// non-default libonnxruntime.so) -- mirroring internal/facerec's
// FACEREC_TEST_MODELS and internal/voicerec's VOICEREC_TEST_MODELS
// conventions. Neither the model nor the native library is bundled with
// this repo, so this test is opt-in only.
func requireTestModel(t *testing.T) (ortLib, modelPath string) {
	t.Helper()
	modelPath = os.Getenv("SILEROVAD_TEST_MODEL")
	if modelPath == "" {
		t.Skip("skipping model-backed silerovad test: set SILEROVAD_TEST_MODEL (and optionally " +
			"SILEROVAD_TEST_ORT_LIB) to a real silero_vad.onnx model/libonnxruntime.so to run it")
	}
	ortLib = os.Getenv("SILEROVAD_TEST_ORT_LIB")
	return ortLib, modelPath
}

// requireBenchModel is requireTestModel for a benchmark -- *testing.B has
// the same Skip/Helper surface as *testing.T, so the env-var convention
// carries over unchanged.
func requireBenchModel(b *testing.B) (ortLib, modelPath string) {
	b.Helper()
	modelPath = os.Getenv("SILEROVAD_TEST_MODEL")
	if modelPath == "" {
		b.Skip("skipping model-backed silerovad benchmark: set SILEROVAD_TEST_MODEL (and optionally " +
			"SILEROVAD_TEST_ORT_LIB) to a real silero_vad.onnx model/libonnxruntime.so to run it")
	}
	ortLib = os.Getenv("SILEROVAD_TEST_ORT_LIB")
	return ortLib, modelPath
}

// BenchmarkVADFeed measures Feed's per-call cost for one real listener-sized
// chunk (4096 bytes = 2048 int16 samples = ~42.7ms @48kHz, matching the mic
// stream's actual chunk size and run.go's real defaults), to compare against
// internal/audiolevel's BenchmarkVADFeed for the same chunk size/cadence.
func BenchmarkVADFeed(b *testing.B) {
	ortLib, modelPath := requireBenchModel(b)
	v, err := NewVAD(ortLib, modelPath, "", 0.5, 900, 200, 48000)
	if err != nil {
		b.Fatalf("NewVAD: %v", err)
	}
	defer v.Close()

	chunk := make([]byte, 4096)
	for i := 0; i < len(chunk); i += 2 {
		binary.LittleEndian.PutUint16(chunk[i:], uint16(int16(200)))
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v.Feed(chunk)
	}
}

// TestVADSpeechClip is the model-backed test (opt-in via SILEROVAD_TEST_MODEL,
// see requireTestModel). It always checks that pure silence neither triggers a
// start nor scores anywhere near the speech threshold. When
// SILERO_TEST_SPEECH_PCM points at a raw 16kHz signed-16-bit-LE mono clip of
// real speech (scripts/run-tests.sh synthesizes one with piper), it also
// asserts that clip -- followed by trailing silence -- confirms both a start
// and an end and drives the probability high.
//
// The real-speech assertion is what actually exercises the audio front-end: a
// pure tone (a sine wave) is NOT speech and Silero correctly scores it low, so
// a tone can't stand in for speech here. And silence stays near zero even with
// a broken front-end -- e.g. omitting Silero v5's required 64-sample context
// prepend collapses real-speech probability from ~1.0 to ~0.17 while leaving
// silence untouched -- so only the real-speech check catches that class of bug.
func TestVADSpeechClip(t *testing.T) {
	ortLib, modelPath := requireTestModel(t)

	newVAD := func() *VAD {
		v, err := NewVAD(ortLib, modelPath, "", 0.5, 300, 100, 16000)
		if err != nil {
			t.Fatalf("NewVAD: %v", err)
		}
		return v
	}

	// Silence: never triggers, probability stays well below threshold.
	v := newVAD()
	defer v.Close()
	if sawStart, _, silenceMax := feedPCM(v, synthPCM16(0, 1.0, 16000, 0)); sawStart {
		t.Error("silence spuriously confirmed a speech start")
	} else if silenceMax >= 0.5 {
		t.Errorf("silence peak probability = %.3f, want well below the 0.5 threshold", silenceMax)
	}

	// Real speech (if provided): confirms a start, scores high, and confirms an
	// end once the trailing silence is fed.
	speechPath := os.Getenv("SILERO_TEST_SPEECH_PCM")
	if speechPath == "" {
		t.Log("SILERO_TEST_SPEECH_PCM unset -- ran silence-only checks; set it (raw 16kHz s16le mono) for the real-speech assertion")
		return
	}
	speech, err := os.ReadFile(speechPath)
	if err != nil {
		t.Fatalf("read SILERO_TEST_SPEECH_PCM: %v", err)
	}
	v2 := newVAD()
	defer v2.Close()
	clip := append(append([]byte(nil), speech...), synthPCM16(0, 1.0, 16000, 0)...)
	start, end, speechMax := feedPCM(v2, clip)
	if !start {
		t.Errorf("real speech did not confirm a start (peak prob=%.3f) -- audio front-end may be broken", speechMax)
	}
	if !end {
		t.Error("trailing silence after speech did not confirm an end")
	}
	if speechMax < 0.8 {
		t.Errorf("real-speech peak probability = %.3f, want >=0.8 (a broken front-end collapses this)", speechMax)
	}
}

// feedPCM feeds pcm through v in ~100ms chunks, reporting whether a start and
// an end were seen and the peak per-window speech probability.
func feedPCM(v *VAD, pcm []byte) (sawStart, sawEnd bool, maxProb float64) {
	const chunk = 3200
	for off := 0; off < len(pcm); off += chunk {
		end := off + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		s, e := v.Feed(pcm[off:end])
		sawStart = sawStart || s
		sawEnd = sawEnd || e
		if v.lastProb > maxProb {
			maxProb = v.lastProb
		}
	}
	return
}

// synthPCM16 synthesizes seconds of a sine tone at freqHz (or silence, if
// amplitude is 0) at sampleRate, as raw little-endian 16-bit PCM.
func synthPCM16(freqHz, seconds float64, sampleRate int, amplitude int16) []byte {
	n := int(float64(sampleRate) * seconds)
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(float64(amplitude) * math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)))
		pcm[i*2] = byte(uint16(v))
		pcm[i*2+1] = byte(uint16(v) >> 8)
	}
	return pcm
}

// TestProbsSpeechWAV runs Probs on a real 16 kHz mono speech WAV (path in
// SILERO_TEST_SPEECH_WAV, upsampled 3x to 48 kHz by sample repetition) and
// expects at least one frame above 0.6, then checks the VAD is reusable
// (deterministic on a second call, i.e. state was reset).
func TestProbsSpeechWAV(t *testing.T) {
	ortLib, modelPath := requireTestModel(t)
	wavPath := os.Getenv("SILERO_TEST_SPEECH_WAV")
	if wavPath == "" {
		t.Skip("set SILERO_TEST_SPEECH_WAV to a 16 kHz mono s16le WAV to run")
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil || len(wav) < 44 {
		t.Skipf("speech WAV unavailable: %v", err)
	}
	pcm16 := wav[44:]
	pcm48 := make([]byte, 0, len(pcm16)*3)
	for i := 0; i+1 < len(pcm16); i += 2 {
		for k := 0; k < 3; k++ {
			pcm48 = append(pcm48, pcm16[i], pcm16[i+1])
		}
	}
	v, err := NewVAD(ortLib, modelPath, "", 0.6, 500, 250, 48000)
	if err != nil {
		t.Fatalf("NewVAD: %v", err)
	}
	defer v.Close()
	probs, err := v.Probs(pcm48)
	if err != nil {
		t.Fatalf("Probs: %v", err)
	}
	if want := len(pcm16) / 2 / 512; len(probs) != want {
		t.Fatalf("len(probs) = %d, want %d", len(probs), want)
	}
	var max float32
	for _, p := range probs {
		if p > max {
			max = p
		}
	}
	if max <= 0.6 {
		t.Fatalf("max prob = %.3f, want > 0.6", max)
	}
	again, err := v.Probs(pcm48)
	if err != nil || len(again) != len(probs) {
		t.Fatalf("second Probs: %v len %d", err, len(again))
	}
	for i := range probs {
		if probs[i] != again[i] {
			t.Fatalf("frame %d differs on reuse: %v vs %v", i, probs[i], again[i])
		}
	}
	t.Logf("frames=%d max=%.3f", len(probs), max)
}
