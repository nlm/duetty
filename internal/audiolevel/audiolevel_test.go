package audiolevel

import (
	"encoding/binary"
	"math"
	"testing"
)

// synthChunk builds a synthetic signed 16-bit little-endian mono PCM chunk
// of exactly durationMS at sampleRate, every sample set to a constant
// amplitude equal to rms -- RMS of a constant-amplitude signal is that same
// amplitude, so this gives callers exact control over what Feed measures.
// durationMS/sampleRate must divide evenly (as they do for every duration
// used below) so the chunk's actual duration exactly matches durationMS.
func synthChunk(t *testing.T, rms, durationMS float64, sampleRate int) []byte {
	t.Helper()
	n := int(durationMS / 1000 * float64(sampleRate))
	if n < 1 {
		n = 1
	}
	amp := uint16(int16(rms))
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(buf[2*i:], amp)
	}
	return buf
}

func TestWarmStartNeverStartsAndSeedsFloor(t *testing.T) {
	const sampleRate = 48000
	v := NewVAD(sampleRate, 2.0, 50, 300, 200, 0.5)
	loud := synthChunk(t, 5000, 50, sampleRate) // well above any plausible threshold

	elapsed := 0.0
	for elapsed < warmupMS {
		started, ended := v.Feed(loud)
		if started || ended {
			t.Fatalf("Feed during warmup (elapsed=%.0fms) = (started=%v, ended=%v), want (false, false) even for loud audio", elapsed, started, ended)
		}
		elapsed += 50
	}

	if v.NoiseFloor() == 0 {
		t.Fatalf("NoiseFloor() = 0 after warmup, want non-zero (should have been seeded from the warmup audio)")
	}
}

// TestResetClearsSpeakingButKeepsCalibration checks Reset's documented
// contract: in-progress utterance state (speaking, the onset/silence
// debounce accumulators) is cleared, but the learned noise floor survives
// -- used by vad.Listener.SetMuted when it finalizes an in-progress
// utterance early instead of waiting for a natural "ended".
func TestResetClearsSpeakingButKeepsCalibration(t *testing.T) {
	const sampleRate = 48000
	v := NewVAD(sampleRate, 2.0, 50, 300, 200, 0.5)
	quiet := synthChunk(t, 10, 50, sampleRate)
	elapsed := 0.0
	for elapsed < warmupMS {
		v.Feed(quiet)
		elapsed += 50
	}
	floorBeforeSpeaking := v.NoiseFloor()

	loud := synthChunk(t, 5000, 250, sampleRate) // exceeds the 200ms onset in one chunk
	started, _ := v.Feed(loud)
	if !started || !v.Speaking() {
		t.Fatal("test setup: expected loud audio to confirm speech start")
	}

	v.Reset()

	if v.Speaking() {
		t.Error("Speaking() = true after Reset, want false")
	}
	if v.NoiseFloor() != floorBeforeSpeaking {
		t.Errorf("NoiseFloor() = %v after Reset, want unchanged at %v (calibration state must survive Reset)", v.NoiseFloor(), floorBeforeSpeaking)
	}

	// A fresh loud chunk shorter than the onset debounce must not
	// immediately report started again -- Reset must clear the onset
	// accumulator too, not just the speaking flag.
	startedAgain, _ := v.Feed(synthChunk(t, 5000, 50, sampleRate))
	if startedAgain {
		t.Error("Feed reported started right after Reset from a sub-onset-length chunk, want it to require fresh onset debounce")
	}
}

func TestOnsetHysteresisBandHoldsAccumulator(t *testing.T) {
	const sampleRate = 48000
	newWarmedVAD := func() *VAD {
		v := NewVAD(sampleRate, 2.0, 50, 300, 200, 0.5)
		v.noiseFloor = 100 // start = max(100*2, 50) = 200; keep = 200*0.5 = 100
		v.warmedMS = warmupMS
		return v
	}

	loud := synthChunk(t, 300, 100, sampleRate) // above start (200)
	band := synthChunk(t, 150, 100, sampleRate) // between keep (100) and start (200)
	quiet := synthChunk(t, 50, 100, sampleRate) // below keep (100)

	t.Run("band chunk between loud chunks still confirms start", func(t *testing.T) {
		v := newWarmedVAD()
		if started, ended := v.Feed(loud); started || ended {
			t.Fatalf("first loud chunk: got (started=%v, ended=%v), want (false, false)", started, ended)
		}
		if started, _ := v.Feed(band); started {
			t.Fatalf("band chunk: got started=true, want false (should hold, not confirm)")
		}
		// A band chunk is ambient-level (below start), so it should nudge the
		// noise floor upward -- the detector keeps tracking the room -- while
		// still holding the onset accumulator (asserted by the next Feed).
		if v.NoiseFloor() <= 100 {
			t.Fatalf("band chunk should have nudged the noise floor up from 100, got %.1f", v.NoiseFloor())
		}
		started, _ := v.Feed(loud)
		if !started {
			t.Fatalf("second loud chunk: got started=false, want true (band chunk should not have reset the onset accumulator)")
		}
	})

	t.Run("quiet chunk below keep threshold resets the accumulator", func(t *testing.T) {
		v := newWarmedVAD()
		if started, _ := v.Feed(loud); started {
			t.Fatalf("first loud chunk: got started=true, want false")
		}
		if started, _ := v.Feed(quiet); started {
			t.Fatalf("quiet chunk: got started=true, want false")
		}
		// Only one onsetMS/2 worth of loud audio accumulated since the reset --
		// not enough on its own to confirm start.
		if started, _ := v.Feed(loud); started {
			t.Fatalf("loud chunk right after reset: got started=true, want false (accumulator should have been cleared)")
		}
		started, _ := v.Feed(loud)
		if !started {
			t.Fatalf("second loud chunk after reset: got started=false, want true")
		}
	})
}

func TestEndpointHysteresis(t *testing.T) {
	const sampleRate = 48000
	v := NewVAD(sampleRate, 2.0, 50, 300, 200, 0.5)
	v.noiseFloor = 100 // start = 200, keep = 100
	v.warmedMS = warmupMS

	loud := synthChunk(t, 300, 100, sampleRate)
	band := synthChunk(t, 150, 100, sampleRate) // in keep(100)..start(200) band
	quiet := synthChunk(t, 50, 150, sampleRate) // below keep

	// Confirm speech start (two 100ms loud chunks = onsetMS).
	if started, _ := v.Feed(loud); started {
		t.Fatalf("unexpected early start")
	}
	if started, _ := v.Feed(loud); !started {
		t.Fatalf("expected speech to start")
	}
	if !v.Speaking() {
		t.Fatalf("Speaking() = false right after start, want true")
	}

	// A band chunk (below start but at/above keep) must not count toward
	// ending the turn -- it's still plausibly speech.
	if _, ended := v.Feed(band); ended {
		t.Fatalf("band chunk ended the turn, want it to be ignored (still speaking)")
	}
	if !v.Speaking() {
		t.Fatalf("Speaking() = false after a band chunk, want still true")
	}

	// Quiet accumulates toward silenceMS (300ms): first 150ms shouldn't end
	// it, a further band chunk should reset the accumulator, then a fresh
	// 300ms of quiet should end it.
	if _, ended := v.Feed(quiet); ended {
		t.Fatalf("ended after only 150ms of quiet, want silenceMS=300 required")
	}
	if _, ended := v.Feed(band); ended {
		t.Fatalf("band chunk ended the turn, want it to reset the silence accumulator instead")
	}
	if _, ended := v.Feed(quiet); ended {
		t.Fatalf("ended after only 150ms of quiet post-reset, want another 150ms first")
	}
	_, ended := v.Feed(quiet)
	if !ended {
		t.Fatalf("expected the turn to end after sustained quiet audio reaching silenceMS")
	}
	if v.Speaking() {
		t.Fatalf("Speaking() = true after ended, want false")
	}
}

func TestThresholdMath(t *testing.T) {
	tests := []struct {
		name        string
		sensitivity float64
		minRMS      float64
		lowRatio    float64
		noiseFloor  float64
		wantStart   float64
		wantKeep    float64
	}{
		{
			name:        "sensitivity*floor dominates minRMS",
			sensitivity: 2.5,
			minRMS:      30,
			lowRatio:    0.4,
			noiseFloor:  40, // 40*2.5 = 100 > 30
			wantStart:   100,
			wantKeep:    40, // 100*0.4
		},
		{
			name:        "minRMS dominates a near-zero floor",
			sensitivity: 2.5,
			minRMS:      30,
			lowRatio:    0.4,
			noiseFloor:  1, // 1*2.5 = 2.5 < 30
			wantStart:   30,
			wantKeep:    12, // 30*0.4
		},
		{
			name:        "default lowRatio 0.5",
			sensitivity: 1.8,
			minRMS:      80,
			lowRatio:    0.5,
			noiseFloor:  200, // 200*1.8 = 360
			wantStart:   360,
			wantKeep:    180,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVAD(48000, tc.sensitivity, tc.minRMS, 900, 200, tc.lowRatio)
			v.noiseFloor = tc.noiseFloor
			if got := v.StartThreshold(); got != tc.wantStart {
				t.Errorf("StartThreshold() = %v, want %v", got, tc.wantStart)
			}
			if got := v.KeepThreshold(); got != tc.wantKeep {
				t.Errorf("KeepThreshold() = %v, want %v", got, tc.wantKeep)
			}
			if got, want := v.KeepThreshold(), v.StartThreshold()*tc.lowRatio; got != want {
				t.Errorf("KeepThreshold() = %v, want StartThreshold()*lowRatio = %v", got, want)
			}
		})
	}
}

// BenchmarkVADFeed measures Feed's per-call cost for one real listener-sized
// chunk (4096 bytes = 2048 int16 samples = ~42.7ms @48kHz, matching the
// mic stream's actual chunk size), using run.go's real defaults -- a
// baseline to compare against internal/silerovad's BenchmarkVADFeed.
func BenchmarkVADFeed(b *testing.B) {
	const sampleRate = 48000
	chunk := make([]byte, 4096)
	for i := 0; i < len(chunk); i += 2 {
		binary.LittleEndian.PutUint16(chunk[i:], uint16(int16(200)))
	}
	v := NewVAD(sampleRate, 1.8, 80, 900, 200, 0.5)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v.Feed(chunk)
	}
}

func TestVoicedMSCountsOnlyChunksAboveStartThreshold(t *testing.T) {
	const sampleRate = 48000
	v := NewVAD(sampleRate, 2.0, 50, 300, 200, 0.5)
	quiet := synthChunk(t, 10, 50, sampleRate)
	elapsed := 0.0
	for elapsed < warmupMS {
		v.Feed(quiet)
		if got := v.VoicedMS(); got != 0 {
			t.Fatalf("VoicedMS() = %v during warmup, want 0", got)
		}
		elapsed += 50
	}

	v.Feed(synthChunk(t, 5000, 50, sampleRate))
	if got := v.VoicedMS(); math.Abs(got-50) > 0.01 {
		t.Errorf("VoicedMS() = %v after a loud 50ms chunk, want 50", got)
	}
	// Between KeepThreshold and StartThreshold: keeps an utterance open,
	// but isn't counted as speech.
	v.Feed(synthChunk(t, (v.KeepThreshold()+v.StartThreshold())/2, 50, sampleRate))
	if got := v.VoicedMS(); got != 0 {
		t.Errorf("VoicedMS() = %v for a hysteresis-band chunk, want 0", got)
	}
	v.Feed(quiet)
	if got := v.VoicedMS(); got != 0 {
		t.Errorf("VoicedMS() = %v after a quiet chunk, want 0", got)
	}
}
