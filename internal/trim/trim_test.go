package trim

import (
	"encoding/binary"
	"testing"
	"time"
)

// ramp returns n samples (s16le) whose value is the sample index (mod 2^16).
func ramp(n int) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(i))
	}
	return b
}

func sampleAt(b []byte, i int) int { return int(binary.LittleEndian.Uint16(b[i*2:])) }

func voiced(n int, from, to int) []float32 {
	p := make([]float32, n)
	for i := from; i <= to; i++ {
		p[i] = 0.9
	}
	return p
}

func TestTrimKeepsVoicedPlusMargin(t *testing.T) {
	pcm := ramp(96000) // 2 s
	probs := voiced(62, 20, 30)
	out := Trim(pcm, probs, 0.6, 150*time.Millisecond)
	margin := 14400
	wantStart := 20*frameBytes - margin
	wantEnd := 31*frameBytes + margin
	if len(out) != wantEnd-wantStart {
		t.Fatalf("len = %d, want %d", len(out), wantEnd-wantStart)
	}
	if len(out)%2 != 0 || wantStart%2 != 0 {
		t.Fatal("not sample aligned")
	}
	if got := sampleAt(out, 0); got != wantStart/2 {
		t.Fatalf("first sample = %d, want %d", got, wantStart/2)
	}
	if got := sampleAt(out, len(out)/2-1); got != wantEnd/2-1 {
		t.Fatalf("last sample = %d, want %d", got, wantEnd/2-1)
	}
}

func TestTrimClampsAtEdges(t *testing.T) {
	pcm := ramp(48000)
	probs := voiced(31, 0, 30)
	out := Trim(pcm, probs, 0.6, 150*time.Millisecond)
	if len(out) != len(pcm) || sampleAt(out, 0) != 0 {
		t.Fatalf("len = %d, want %d starting at 0", len(out), len(pcm))
	}
}

func TestTrimNoVoice(t *testing.T) {
	if out := Trim(ramp(48000), make([]float32, 31), 0.6, 150*time.Millisecond); out != nil {
		t.Fatalf("got %d bytes, want nil", len(out))
	}
	if out := Trim(ramp(48000), nil, 0.6, 0); out != nil {
		t.Fatal("want nil for no probs")
	}
}
