package vad

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlm/poly/internal/log"
)

// fakeVAD is a scripted VoiceDetector: each Feed call consumes the next
// scripted (started, ended) result (or (false, false) once the script is
// exhausted), and Speaking() reflects that script rather than any real
// audio analysis.
type fakeVAD struct {
	script   []fakeVADStep
	idx      int
	resetIdx int // Speaking() only considers script[resetIdx:idx] -- see Reset
	label    string

	fed        [][]byte
	resetCalls int
}

type fakeVADStep struct {
	started, ended bool
	voiced         float64 // ms of this chunk reported by VoicedMS
}

func (f *fakeVAD) Feed(pcm []byte) (started, ended bool) {
	f.fed = append(f.fed, pcm)
	if f.idx >= len(f.script) {
		return false, false
	}
	step := f.script[f.idx]
	f.idx++
	return step.started, step.ended
}

func (f *fakeVAD) Speaking() bool {
	// Speaking mirrors whatever the most recently consumed step implied:
	// true from a started (not yet ended) step onward. Recomputed from the
	// script position (starting at resetIdx, not always 0 -- see Reset)
	// rather than tracked incrementally so it stays correct regardless of
	// call order relative to Feed.
	speaking := false
	for i := f.resetIdx; i < f.idx; i++ {
		if f.script[i].started {
			speaking = true
		}
		if f.script[i].ended {
			speaking = false
		}
	}
	return speaking
}

func (f *fakeVAD) DetectionLabel() string { return f.label }

// VoicedMS reports the most recently consumed step's voiced ms, making
// fakeVAD a SpeechMeter.
func (f *fakeVAD) VoicedMS() float64 {
	if f.idx == 0 || f.idx > len(f.script) {
		return 0
	}
	return f.script[f.idx-1].voiced
}

// Reset marks every script step consumed so far as no longer contributing
// to Speaking() -- mirroring the real VoiceDetector implementations
// forgetting they were mid-utterance, without needing to actually rewind
// or replay the script.
func (f *fakeVAD) Reset() {
	f.resetIdx = f.idx
	f.resetCalls++
}

func chunkOf(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

// waitUntil polls cond (roughly every millisecond) until it returns true or
// timeout elapses, returning whether it succeeded. Used instead of a single
// fixed time.Sleep after sending a chunk on an unbuffered channel: Run's
// receive unblocks the send before it has locked and processed the chunk,
// so a fixed sleep that's merely "usually enough" is a latent flake source
// on a loaded machine.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// TestListenerCapturesUtteranceWithPrebuffer drives a start/middle/end
// sequence and verifies the delivered utterance is exactly the rolling
// pre-buffer (captured before the VAD confirmed speech) plus every chunk
// from the start chunk through the end chunk.
func TestListenerCapturesUtteranceWithPrebuffer(t *testing.T) {
	v := &fakeVAD{
		label: "energy",
		script: []fakeVADStep{
			{},              // pre-speech chunk 1: buffered into preBuf
			{},              // pre-speech chunk 2: buffered into preBuf
			{started: true}, // speech starts
			{},              // mid-utterance chunk
			{ended: true},   // speech ends
		},
	}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 1000) // generous prebuf window, nothing trimmed

	go l.Run()

	pre1 := chunkOf(4, 1)
	pre2 := chunkOf(4, 2)
	start := chunkOf(4, 3)
	mid := chunkOf(4, 4)
	end := chunkOf(4, 5)

	chunks <- pre1
	chunks <- pre2
	chunks <- start
	chunks <- mid

	select {
	case <-l.Utterances():
		t.Fatal("utterance delivered before the end chunk was fed")
	case <-time.After(20 * time.Millisecond):
	}

	chunks <- end

	select {
	case got, ok := <-l.Utterances():
		if !ok {
			t.Fatal("Utterances() closed unexpectedly")
		}
		want := append(append(append(append([]byte{}, pre1...), pre2...), start...), mid...)
		want = append(want, end...)
		if string(got) != string(want) {
			t.Errorf("utterance = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the utterance")
	}

	close(chunks)
}

// TestListenerPreBufTrimsToWindow checks the rolling pre-buffer discards
// its oldest bytes once it exceeds preBufMS worth of audio, so a long
// silence before an utterance doesn't grow the buffer unboundedly or
// prepend stale audio.
func TestListenerPreBufTrimsToWindow(t *testing.T) {
	v := &fakeVAD{
		script: []fakeVADStep{
			{}, {}, {}, // three pre-speech chunks, more than the window allows
			{started: true},
			{ended: true},
		},
	}
	chunks := make(chan []byte)
	// preBufMaxBytes = SampleRate * 2 * preBufMS / 1000 = 48000*2*0.1/1000 = 9.6 -> 9 bytes.
	l := NewListener(chunks, v, 0.1)
	go l.Run()

	c1 := chunkOf(4, 1)
	c2 := chunkOf(4, 2)
	c3 := chunkOf(4, 3)
	start := chunkOf(2, 9)

	chunks <- c1
	chunks <- c2
	chunks <- c3
	chunks <- start
	chunks <- chunkOf(1, 0) // ended chunk

	select {
	case got := <-l.Utterances():
		// Only the tail of the pre-buffer within the ~9-byte window should
		// survive, followed by the start and end chunks.
		if len(got) >= len(c1)+len(c2)+len(c3)+len(start)+1 {
			t.Errorf("utterance length = %d, want the stale prebuffer trimmed away", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the utterance")
	}
	close(chunks)
}

// TestListenerMutedDiscardsChunks verifies muted audio never reaches the
// VAD at all -- the whole point of muting during a spoken reply so the mic
// doesn't "hear" the app's own voice.
func TestListenerMutedDiscardsChunks(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	l.SetMuted(true, true)
	if !l.IsMuted() {
		t.Fatal("IsMuted() = false after SetMuted(true)")
	}

	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2)

	// Give Run's goroutine a moment to process (and discard) the chunks.
	time.Sleep(20 * time.Millisecond)

	l.mu.Lock()
	fedCount := len(v.fed)
	l.mu.Unlock()
	if fedCount != 0 {
		t.Errorf("vad.Feed was called %d times while muted, want 0", fedCount)
	}

	l.SetMuted(false, true)
	if l.IsMuted() {
		t.Fatal("IsMuted() = true after SetMuted(false)")
	}
	close(chunks)
}

// TestListenerSetMutedClearsPreBuf checks muting drops any rolling
// pre-buffer accumulated so far, so unmuting doesn't resurrect stale
// pre-speech audio into the next utterance.
func TestListenerSetMutedClearsPreBuf(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 1000)
	go l.Run()

	chunks <- chunkOf(4, 1)
	bufferedLen := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.preBuf)
	}
	if !waitUntil(time.Second, func() bool { return bufferedLen() > 0 }) {
		t.Fatal("test setup: expected the pre-speech chunk to have been buffered")
	}

	l.SetMuted(true, true)
	l.mu.Lock()
	after := len(l.preBuf)
	l.mu.Unlock()
	if after != 0 {
		t.Errorf("preBuf len after SetMuted(true) = %d, want 0", after)
	}
	close(chunks)
}

// TestListenerSetMutedMidUtteranceFinalizesUtterance is the regression
// test for a reported bug: hitting mute while actively speaking (the
// VoiceDetector still mid-utterance) used to silently discard the partial
// capture instead of processing what had already been said. Muting now
// finalizes it as a real utterance, delivered on Utterances() exactly
// like a naturally-ended one, and resets the detector so it no longer
// believes it's mid-utterance (checked both via the underlying
// VoiceDetector's own Reset call and via Listener.Speaking() reporting
// false afterward) -- otherwise unmuting later would still splice a
// pre-mute fragment onto whatever audio comes after.
func TestListenerSetMutedMidUtteranceFinalizesUtterance(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	start := chunkOf(4, 1)
	chunks <- start // starts the utterance; l.buf becomes non-empty
	bufLen := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.buf)
	}
	if !waitUntil(time.Second, func() bool { return bufLen() > 0 }) {
		t.Fatal("test setup: expected the started chunk to have been buffered into buf")
	}

	l.SetMuted(true, true)

	select {
	case got, ok := <-l.Utterances():
		if !ok {
			t.Fatal("Utterances() closed unexpectedly")
		}
		if string(got) != string(start) {
			t.Errorf("finalized utterance = %v, want %v (the partial capture, not discarded)", got, start)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the mute-finalized utterance")
	}

	if got := bufLen(); got != 0 {
		t.Errorf("buf len after SetMuted(true) mid-utterance = %d, want 0 (moved into the finalized utterance, not left behind)", got)
	}
	l.mu.Lock()
	resetCalls := v.resetCalls
	l.mu.Unlock()
	if resetCalls != 1 {
		t.Errorf("VoiceDetector.Reset called %d times, want 1", resetCalls)
	}
	if l.Speaking() {
		t.Error("Listener.Speaking() = true after a mute-triggered finalize, want false (detector must not still believe it's mid-utterance)")
	}
	close(chunks)
}

// TestListenerSetMutedHarnessStyleNeverFinalizes is the regression test
// for a real bug a review caught: an automatic (harness-style) mute --
// finalizeIfSpeaking=false -- must never promote a mid-utterance capture
// into a delivered turn, even though it still must reset the detector.
// Only the user's own deliberate mute (finalizeIfSpeaking=true, see
// TestListenerSetMutedMidUtteranceFinalizesUtterance above) should ever
// finalize -- otherwise a stray VAD onset accumulating in the brief
// window before an automatic mute lands (ambient noise, a scheduler
// stall right as a turn starts) could get promoted into a bogus turn
// instead of silently discarded.
func TestListenerSetMutedHarnessStyleNeverFinalizes(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	chunks <- chunkOf(4, 1) // starts the utterance
	if !waitUntil(time.Second, l.Speaking) {
		t.Fatal("test setup: expected the started chunk to confirm speaking")
	}

	l.SetMuted(true, false) // harness-style mute: must discard, not finalize

	select {
	case got := <-l.Utterances():
		t.Errorf("an utterance was delivered (%v), want none -- finalizeIfSpeaking was false", got)
	case <-time.After(50 * time.Millisecond):
	}

	l.mu.Lock()
	resetCalls := v.resetCalls
	bufLen := len(l.buf)
	l.mu.Unlock()
	if resetCalls != 1 {
		t.Errorf("VoiceDetector.Reset called %d times, want 1 (still resets even without finalizing, to avoid the stale-mid-utterance-on-unmute splice bug)", resetCalls)
	}
	if bufLen != 0 {
		t.Errorf("buf len = %d, want 0 (discarded, not finalized)", bufLen)
	}
	if l.Speaking() {
		t.Error("Listener.Speaking() = true after a non-finalizing mid-utterance mute, want false")
	}
	close(chunks)
}

// TestListenerSetMutedFlushesDeliveredInOrder checks that two separate
// mid-utterance-mute finalizes are delivered in the order they happened,
// not whatever order two independently-scheduled goroutines might
// otherwise race to deliver them in -- see Run/SetMuted's doc comments
// for why pendingFlush is an ordered queue drained solely by Run's own
// goroutine specifically to make this guaranteed by construction, not
// just usually true.
func TestListenerSetMutedFlushesDeliveredInOrder(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	// Each cycle drains its own flush before the next one starts: Run's
	// single goroutine can't accept a new chunk while it's still blocked
	// delivering a pending flush to Utterances() (exactly like it already
	// couldn't for a naturally-ended utterance, before this feature
	// existed) -- a real mic-feed goroutine is unaffected by this (it's
	// independent of whatever's draining Utterances()), but this
	// single-goroutine test would deadlock itself against Run() if it
	// tried to feed a second chunk before draining the first flush.
	first := chunkOf(4, 1)
	chunks <- first // starts utterance #1
	if !waitUntil(time.Second, l.Speaking) {
		t.Fatal("test setup: expected the first started chunk to confirm speaking")
	}
	l.SetMuted(true, true) // finalizes utterance #1

	var got [][]byte
	select {
	case u, ok := <-l.Utterances():
		if !ok {
			t.Fatal("Utterances() closed unexpectedly")
		}
		got = append(got, u)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for utterance 1")
	}

	l.SetMuted(false, true) // unmute

	second := chunkOf(4, 2)
	chunks <- second // starts utterance #2
	if !waitUntil(time.Second, l.Speaking) {
		t.Fatal("test setup: expected the second started chunk to confirm speaking")
	}
	l.SetMuted(true, true) // finalizes utterance #2

	select {
	case u, ok := <-l.Utterances():
		if !ok {
			t.Fatal("Utterances() closed unexpectedly")
		}
		got = append(got, u)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for utterance 2")
	}

	if len(got) != 2 || string(got[0]) != string(first) || string(got[1]) != string(second) {
		t.Errorf("delivered utterances = %v, want [%v, %v] in that order", got, first, second)
	}
	close(chunks)
}

// TestListenerShutdownDuringPendingFlushDoesNotPanic is the regression
// test for a real panic this exact scenario used to trigger during
// development: SetMuted's flush-delivery goroutine sending directly on
// Utterances() could race Run's own close(Utterances()) once the input
// channel closes before anything ever drains that flush ("send on closed
// channel"). Closing chunks immediately after a mid-utterance mute, with
// nothing ever draining Utterances(), must not panic -- Run's shutdown is
// expected to block forever waiting out that undeliverable flush (see
// SetMuted/Run's doc comments), which this test never joins on, matching
// how a real process shutdown would just reclaim the stuck goroutine.
func TestListenerShutdownDuringPendingFlushDoesNotPanic(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	chunks <- chunkOf(4, 1) // starts the utterance
	if !waitUntil(time.Second, l.Speaking) {
		t.Fatal("test setup: expected the started chunk to confirm speaking")
	}

	l.SetMuted(true, true) // queues a flush for Run to deliver, but nothing ever drains it
	close(chunks)          // Run's loop exits and its shutdown races that pending flush

	// No assertion beyond "the test binary is still alive" is possible or
	// needed here: the historical bug crashed the whole process with an
	// unrecovered panic right around this point, so simply reaching this
	// line already proves the fix.
	time.Sleep(50 * time.Millisecond)
}

// TestListenerSetMutedRejectsFlushOnceClosing is a deterministic,
// white-box regression test for the exact mechanism that fixes a real
// data-loss bug a review caught: with closing already set (simulating
// Run's shutdown having already committed to closing utterances),
// SetMuted must not enqueue a mid-utterance flush at all, even though
// Speaking() is true and finalizeIfSpeaking is true -- appending it would
// silently queue an utterance nothing will ever drain. This deliberately
// doesn't run Run() at all, so the outcome is pinned down exactly rather
// than depending on how a real race between two goroutines resolves.
func TestListenerSetMutedRejectsFlushOnceClosing(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	// Manually drive Feed to populate buf and mark speaking, bypassing
	// Run() entirely so this test controls every step.
	_, _ = v.Feed(chunkOf(4, 1))
	l.buf = chunkOf(4, 1)
	l.closing = true

	l.SetMuted(true, true)

	if len(l.pendingFlush) != 0 {
		t.Errorf("pendingFlush = %v, want empty -- a flush must never be enqueued once closing is set", l.pendingFlush)
	}
	if v.resetCalls != 1 {
		t.Errorf("VoiceDetector.Reset called %d times, want 1 -- the detector must still reset even when the flush itself is rejected", v.resetCalls)
	}
	close(chunks)
}

// TestListenerDrainPendingFlushesAndCloseDeliversBeforeClosing checks
// drainPendingFlushesAndClose's other half of the same invariant: a flush
// already sitting in the queue when shutdown begins must be fully
// delivered before closing is set and utterances is closed, not raced
// against or skipped.
func TestListenerDrainPendingFlushesAndCloseDeliversBeforeClosing(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	want := chunkOf(4, 7)
	l.pendingFlush = [][]byte{want}

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.drainPendingFlushesAndClose()
	}()

	select {
	case got, ok := <-l.Utterances():
		if !ok {
			t.Fatal("Utterances() closed before delivering the queued flush")
		}
		if string(got) != string(want) {
			t.Errorf("delivered utterance = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the pre-queued flush to be delivered")
	}

	select {
	case _, ok := <-l.Utterances():
		if ok {
			t.Error("Utterances() delivered a second value, want it closed after the one queued flush")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Utterances() to close after the flush was delivered")
	}

	<-done
	l.mu.Lock()
	closing := l.closing
	l.mu.Unlock()
	if !closing {
		t.Error("closing = false after drainPendingFlushesAndClose returned, want true")
	}
}

// TestListenerShutdownRaceNeverHangsOrPanics is a stress test for the
// data-loss bug a review empirically confirmed (roughly a quarter of
// iterations silently lost the flush under concurrent load, with no data
// race and no panic -- a pure logic/ordering bug the race detector can't
// catch). It can't distinguish "delivered" from "correctly rejected
// because closing already won the race" from the outside (both are valid
// outcomes -- see TestListenerSetMutedRejectsFlushOnceClosing and
// TestListenerDrainPendingFlushesAndCloseDeliversBeforeClosing for the
// deterministic proof of the actual mechanism), so this only asserts the
// coarser property that matters at this level: racing SetMuted against
// close(chunks) many times over never hangs or panics.
func TestListenerShutdownRaceNeverHangsOrPanics(t *testing.T) {
	const iterations = 300
	for i := 0; i < iterations; i++ {
		v := &fakeVAD{script: []fakeVADStep{{started: true}}}
		chunks := make(chan []byte)
		l := NewListener(chunks, v, 500)
		go l.Run()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range l.Utterances() {
			}
		}()

		chunks <- chunkOf(4, 1) // starts the utterance
		if !waitUntil(time.Second, l.Speaking) {
			t.Fatalf("iteration %d: test setup: expected Speaking() after the started chunk", i)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			l.SetMuted(true, true)
		}()
		go func() {
			defer wg.Done()
			close(chunks)
		}()
		wg.Wait()

		select {
		case <-drained:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: Utterances() never closed within 1s after the shutdown race", i)
		}
	}
}

// TestListenerSetMutedBeforeUtteranceStartedStillDiscards checks the other
// half of SetMuted's behavior: muting while the VoiceDetector does NOT
// consider the user mid-utterance (nothing but the rolling pre-buffer
// accumulated) has nothing worth finalizing, so it's discarded exactly as
// before -- no utterance should be delivered, and Reset must not even be
// called (there's nothing to reset).
func TestListenerSetMutedBeforeUtteranceStartedStillDiscards(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{}}} // never starts
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	chunks <- chunkOf(4, 1)
	preBufLen := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.preBuf)
	}
	if !waitUntil(time.Second, func() bool { return preBufLen() > 0 }) {
		t.Fatal("test setup: expected the chunk to have been buffered into preBuf")
	}

	l.SetMuted(true, true)

	select {
	case got := <-l.Utterances():
		t.Errorf("an utterance was delivered (%v), want none -- nothing had started yet", got)
	case <-time.After(20 * time.Millisecond):
	}

	l.mu.Lock()
	resetCalls := v.resetCalls
	l.mu.Unlock()
	if resetCalls != 0 {
		t.Errorf("VoiceDetector.Reset called %d times, want 0 (nothing was mid-utterance)", resetCalls)
	}
	close(chunks)
}

// TestListenerSpeakingReflectsVAD checks Listener.Speaking() just forwards
// the underlying VoiceDetector's Speaking(), independent of the mute state
// tracked separately by IsMuted().
func TestListenerSpeakingReflectsVAD(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	if l.Speaking() {
		t.Error("Speaking() = true before any chunk was fed")
	}
	chunks <- chunkOf(4, 1)
	if !waitUntil(time.Second, l.Speaking) {
		t.Error("Speaking() = false after a started chunk was fed")
	}
	close(chunks)
}

// TestListenerClosesUtterancesWhenInputCloses checks Run's contract: once
// the input chunk channel closes, Utterances() closes too rather than
// blocking forever.
func TestListenerClosesUtterancesWhenInputCloses(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()

	close(chunks)

	select {
	case _, ok := <-l.Utterances():
		if ok {
			t.Error("Utterances() delivered a value instead of closing")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Utterances() to close")
	}
}

// TestListenerToleratesInputThatNeverActivates proves the invariant
// internal/app relies on for -stt-backend none: a chunks channel that is
// simply never sent to and never closed (standing in for a mic that's
// deliberately never opened, rather than one that failed) must leave
// Utterances() neither producing a value nor closing, indefinitely -- Run
// must not busy-loop, panic, or otherwise misbehave against a permanently
// idle input. This matters specifically because Utterances() closing is not
// a benign "nothing to report" signal to every consumer: internal/app's own
// runtime.loop() treats a closed Utterances() channel as "shut the whole
// session down" (its select case returns from loop() on !ok, the same as
// StreamChat's own channel-close contract elsewhere in this codebase), so
// silently closing it just because no audio ever arrived would end the
// entire session's typed-input path too, not merely disable voice.
func TestListenerToleratesInputThatNeverActivates(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte) // never sent to, never closed
	l := NewListener(chunks, v, 500)
	go l.Run()

	select {
	case v, ok := <-l.Utterances():
		t.Fatalf("Utterances() produced (value=%v, ok=%v) from an input channel that was never touched, want it to stay silent", v, ok)
	case <-time.After(200 * time.Millisecond):
		// Expected: no activity within a bounded wait.
	}
}

// TestListenerOnLevelCalledPerProcessedChunk drives the mic-level meter's
// data source: OnLevel must fire once for every chunk actually fed to the
// vad, so a UI wired to it (see internal/tui.App.SetMicLevel) gets live
// feedback.
func TestListenerOnLevelCalledPerProcessedChunk(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)

	var mu sync.Mutex
	var levels []float64
	l.OnLevel = func(rms float64) {
		mu.Lock()
		levels = append(levels, rms)
		mu.Unlock()
	}
	go l.Run()

	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2)

	ok := waitUntil(time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(levels) == 2
	})
	mu.Lock()
	got := len(levels)
	mu.Unlock()
	if !ok {
		t.Fatalf("OnLevel called %d times, want 2", got)
	}

	close(chunks)
}

// TestListenerOnLevelNotCalledWhileMuted checks OnLevel is skipped right
// alongside vad.Feed for muted chunks (see TestListenerMutedDiscardsChunks)
// -- a mic-level meter driven by this naturally goes quiet while muted,
// rather than needing to be told about mute state separately.
func TestListenerOnLevelNotCalledWhileMuted(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)

	var mu sync.Mutex
	var calls int
	l.OnLevel = func(rms float64) {
		mu.Lock()
		calls++
		mu.Unlock()
	}
	go l.Run()

	l.SetMuted(true, true)
	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2)
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Errorf("OnLevel called %d times while muted, want 0", got)
	}

	close(chunks)
}

// TestListenerOnMuteChangeFiresForEverySetMuted is the regression test for
// a UI badge going stale: SetMuted also runs automatically around every
// turn (mute for the AI's reply, unmute after -- see internal/app's
// runVoiceTurn/runTypedTurn), not just an explicit user
// toggle, so OnMuteChange must fire for all of them, not only ones some
// "toggle" call site happens to also report back itself.
func TestListenerOnMuteChangeFiresForEverySetMuted(t *testing.T) {
	v := &fakeVAD{}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)

	var mu sync.Mutex
	var got []bool
	l.OnMuteChange = func(muted bool) {
		mu.Lock()
		got = append(got, muted)
		mu.Unlock()
	}

	l.SetMuted(true, true)
	l.SetMuted(false, true)
	l.SetMuted(true, true)

	mu.Lock()
	defer mu.Unlock()
	want := []bool{true, false, true}
	if len(got) != len(want) {
		t.Fatalf("OnMuteChange calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("OnMuteChange calls = %v, want %v", got, want)
			break
		}
	}

	close(chunks)
}

// speechHookCounts wires counting OnSpeechStart/OnSpeechAbort hooks.
func speechHookCounts(l *Listener) (starts, aborts *atomic.Int32) {
	starts, aborts = &atomic.Int32{}, &atomic.Int32{}
	l.OnSpeechStart = func() { starts.Add(1) }
	l.OnSpeechAbort = func() { aborts.Add(1) }
	return starts, aborts
}

func TestListenerOnSpeechStartFiresOncePerOnset(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {}, {ended: true}, {started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	starts, aborts := speechHookCounts(l)
	go l.Run()

	chunks <- chunkOf(4, 1) // onset
	chunks <- chunkOf(4, 2)
	chunks <- chunkOf(4, 3) // end: Run blocks delivering the utterance
	<-l.Utterances()
	chunks <- chunkOf(4, 4) // second onset

	if !waitUntil(time.Second, func() bool { return starts.Load() == 2 }) {
		t.Errorf("OnSpeechStart fired %d times, want 2 (one per onset)", starts.Load())
	}
	if got := aborts.Load(); got != 0 {
		t.Errorf("OnSpeechAbort fired %d times, want 0 (the first utterance was delivered)", got)
	}
	close(chunks)
}

func TestListenerOnSpeechStartAndEndInOneChunk(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true, ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	starts, aborts := speechHookCounts(l)
	go l.Run()

	chunks <- chunkOf(4, 1)
	select {
	case <-l.Utterances():
	case <-time.After(time.Second):
		t.Fatal("a start+end chunk delivered no utterance")
	}
	if got := starts.Load(); got != 1 {
		t.Errorf("OnSpeechStart fired %d times, want 1", got)
	}
	if got := aborts.Load(); got != 0 {
		t.Errorf("OnSpeechAbort fired %d times, want 0", got)
	}
	close(chunks)
}

func TestListenerOnSpeechAbortFiresOnDiscardingMute(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	_, aborts := speechHookCounts(l)
	go l.Run()

	chunks <- chunkOf(4, 1)
	if !waitUntil(time.Second, l.Speaking) {
		t.Fatal("test setup: expected the started chunk to confirm speaking")
	}
	l.SetMuted(true, false) // harness-style: discards the onset
	if got := aborts.Load(); got != 1 {
		t.Errorf("OnSpeechAbort fired %d times, want 1", got)
	}
	close(chunks)
}

func TestListenerOnSpeechAbortNotOnFinalizingMute(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	_, aborts := speechHookCounts(l)
	go l.Run()

	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2)
	if !waitUntil(time.Second, l.Speaking) {
		t.Fatal("test setup: expected speaking")
	}
	l.SetMuted(true, true) // user-chosen mute: finalizes, i.e. delivers
	select {
	case <-l.Utterances():
	case <-time.After(time.Second):
		t.Fatal("finalizing mute delivered no utterance")
	}
	if got := aborts.Load(); got != 0 {
		t.Errorf("OnSpeechAbort fired %d times on a finalizing mute, want 0 (delivered, not aborted)", got)
	}
	close(chunks)
}

func TestListenerSpeechHooksSilentWithoutSpeech(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	starts, aborts := speechHookCounts(l)
	go l.Run()

	l.SetMuted(true, false) // nothing in progress: not an abort
	chunks <- chunkOf(4, 1) // muted: discarded, never fed to the vad
	l.SetMuted(false, false)
	time.Sleep(20 * time.Millisecond)

	if got := starts.Load(); got != 0 {
		t.Errorf("OnSpeechStart fired %d times for a muted chunk, want 0", got)
	}
	if got := aborts.Load(); got != 0 {
		t.Errorf("OnSpeechAbort fired %d times with nothing in progress, want 0", got)
	}
	close(chunks)
}

func TestListenerLastEndpointWaitNatural(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SetSilenceMS(900)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2)
	select {
	case <-l.Utterances():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the utterance")
	}
	if got := l.LastEndpointWait(); got != 900*time.Millisecond {
		t.Errorf("LastEndpointWait = %v, want 900ms", got)
	}
}

func TestListenerLastEndpointWaitFlushIsZero(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SetSilenceMS(900)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(4, 1)
	if !waitUntil(time.Second, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.buf) > 0
	}) {
		t.Fatal("test setup: chunk never buffered")
	}
	l.SetMuted(true, true)
	select {
	case <-l.Utterances():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the flushed utterance")
	}
	if got := l.LastEndpointWait(); got != 0 {
		t.Errorf("LastEndpointWait after flush = %v, want 0", got)
	}
}

// chunk100ms is exactly 100 ms of 48 kHz mono s16le audio, so the hold
// logic's quiet-time accounting lands on whole milliseconds in these tests.
const chunk100ms = SampleRate / 10 * 2

// fakeJudge is a scripted TurnJudge: each Predict call consumes the next
// scripted result (repeating the last one once the script runs out),
// recording how many times it was asked and how much audio it saw.
type fakeJudge struct {
	mu      sync.Mutex
	probs   []float32
	errs    []error
	calls   int
	pcmLens []int
}

func (f *fakeJudge) Predict(pcm []byte) (float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	f.pcmLens = append(f.pcmLens, len(pcm))
	if len(f.errs) > 0 {
		return 0, f.errs[min(i, len(f.errs)-1)]
	}
	return f.probs[min(i, len(f.probs)-1)], nil
}

func (f *fakeJudge) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeJudge) PCMLens() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.pcmLens...)
}

// warnSink captures Warnf calls (formatted) so a test can count them.
type warnSink struct {
	mu    sync.Mutex
	warns []string
}

func (s *warnSink) User(string)                        {}
func (s *warnSink) Assistant(string)                   {}
func (s *warnSink) Tool(string, string, time.Duration) {}
func (s *warnSink) Info(string, ...any)                {}
func (s *warnSink) Warnf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warns = append(s.warns, fmt.Sprintf(format, args...))
}

func (s *warnSink) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warns...)
}

// newJudgedListener returns a running Listener with judge j at the
// shipped defaults (threshold 0.5, pause 200, recheck 600, max 2500).
func newJudgedListener(t *testing.T, v *fakeVAD, j *fakeJudge) (chan []byte, *Listener) {
	t.Helper()
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SetSilenceMS(200)
	l.SetTurnJudge(TurnJudgeConfig{Judge: j, Threshold: 0.5, RecheckMS: 600, MaxSilenceMS: 2500})
	return chunks, l
}

func silentSteps(n int) []fakeVADStep { return make([]fakeVADStep, n) }

func expectUtterance(t *testing.T, l *Listener) []byte {
	t.Helper()
	select {
	case u, ok := <-l.Utterances():
		if !ok {
			t.Fatal("Utterances() closed unexpectedly")
		}
		return u
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an utterance")
		return nil
	}
}

func expectNoUtterance(t *testing.T, l *Listener, why string) {
	t.Helper()
	select {
	case u := <-l.Utterances():
		t.Fatalf("utterance (%d bytes) delivered %s", len(u), why)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestTurnJudgeDeliversWhenConfident(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {}, {ended: true}}}
	j := &fakeJudge{probs: []float32{0.9}}
	chunks, l := newJudgedListener(t, v, j)
	go l.Run()
	defer close(chunks)

	for i := range 3 {
		chunks <- chunkOf(chunk100ms, byte(i+1))
	}
	u := expectUtterance(t, l)
	if len(u) != 3*chunk100ms {
		t.Errorf("utterance = %d bytes, want %d", len(u), 3*chunk100ms)
	}
	if got := j.Calls(); got != 1 {
		t.Errorf("judge called %d times, want 1", got)
	}
	if got := j.PCMLens(); len(got) != 1 || got[0] != 3*chunk100ms {
		t.Errorf("judge saw pcm lengths %v, want [%d]", got, 3*chunk100ms)
	}
	if got := l.LastEndpointWait(); got != 200*time.Millisecond {
		t.Errorf("LastEndpointWait = %v, want 200ms (the pause)", got)
	}
}

func TestTurnJudgeWaitsThenResumedSpeechIsSameUtterance(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{
		{},              // pre-speech: pre-buffered
		{started: true}, // first part
		{ended: true},   // pause: judge says 0.2 -> hold
		{},              // in-between silence, still held
		{started: true}, // resumed speech: same utterance
		{ended: true},   // pause again: judge says 0.8 -> deliver
	}}
	j := &fakeJudge{probs: []float32{0.2, 0.8}}
	chunks, l := newJudgedListener(t, v, j)
	starts, aborts := speechHookCounts(l)
	go l.Run()
	defer close(chunks)

	var want []byte
	for i := range 6 {
		c := chunkOf(chunk100ms, byte(i+1))
		want = append(want, c...)
		chunks <- c
		if i == 2 {
			if !waitUntil(time.Second, func() bool { return j.Calls() == 1 }) {
				t.Fatal("judge not asked on the first pause")
			}
			expectNoUtterance(t, l, "after a 0.2 judgment")
		}
	}
	u := expectUtterance(t, l)
	if string(u) != string(want) {
		t.Errorf("utterance = %d bytes, want both parts plus the silence between (%d bytes, pre-buffer included once)", len(u), len(want))
	}
	if got := j.Calls(); got != 2 {
		t.Errorf("judge called %d times, want 2", got)
	}
	if got := j.PCMLens(); len(got) != 2 || got[1] != len(want) {
		t.Errorf("judge saw pcm lengths %v, want the second call to see the whole %d bytes", got, len(want))
	}
	if got := starts.Load(); got != 1 {
		t.Errorf("OnSpeechStart fired %d times, want 1 (resumed speech is the same utterance)", got)
	}
	if got := aborts.Load(); got != 0 {
		t.Errorf("OnSpeechAbort fired %d times, want 0", got)
	}
}

func TestTurnJudgeRechecksOnQuiet(t *testing.T) {
	v := &fakeVAD{script: append([]fakeVADStep{{started: true}, {ended: true}}, silentSteps(20)...)}
	j := &fakeJudge{probs: []float32{0.2, 0.2, 0.9}}
	chunks, l := newJudgedListener(t, v, j)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2) // ended: quiet 200, check #1
	for i := 1; i <= 11; i++ {
		chunks <- chunkOf(chunk100ms, 0) // quiet 300..1300; check #2 at 800
	}
	if !waitUntil(time.Second, func() bool { return j.Calls() == 2 }) {
		t.Fatalf("judge called %d times by quiet 1300ms, want 2", j.Calls())
	}
	expectNoUtterance(t, l, "before the third check")
	chunks <- chunkOf(chunk100ms, 0) // quiet 1400: check #3 -> 0.9
	expectUtterance(t, l)
	if got := j.Calls(); got != 3 {
		t.Errorf("judge called %d times, want 3", got)
	}
	if got := l.LastEndpointWait(); got != 1400*time.Millisecond {
		t.Errorf("LastEndpointWait = %v, want 1400ms (200+600+600)", got)
	}
}

func TestTurnJudgeMaxSilenceDeliversWithoutAsking(t *testing.T) {
	v := &fakeVAD{script: append([]fakeVADStep{{started: true}, {ended: true}}, silentSteps(40)...)}
	j := &fakeJudge{probs: []float32{0.1}}
	chunks, l := newJudgedListener(t, v, j)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	for i := 1; i <= 22; i++ { // quiet up to 2400
		chunks <- chunkOf(chunk100ms, 0)
	}
	expectNoUtterance(t, l, "before max silence")
	chunks <- chunkOf(chunk100ms, 0) // quiet 2500
	expectUtterance(t, l)
	// Checks at 200, 800, 1400, 2000; the 2500 cap delivers without one.
	if got := j.Calls(); got != 4 {
		t.Errorf("judge called %d times, want 4", got)
	}
	if got := l.LastEndpointWait(); got < 2500*time.Millisecond {
		t.Errorf("LastEndpointWait = %v, want >= 2500ms", got)
	}
}

func TestTurnJudgeErrorFailsOpenAndWarnsOnce(t *testing.T) {
	sink := &warnSink{}
	log.SetSink(sink)
	defer log.SetSink(nil)

	v := &fakeVAD{script: []fakeVADStep{{started: true}, {ended: true}, {started: true}, {ended: true}}}
	j := &fakeJudge{errs: []error{errors.New("boom")}}
	chunks, l := newJudgedListener(t, v, j)
	go l.Run()
	defer close(chunks)

	for range 2 {
		chunks <- chunkOf(chunk100ms, 1)
		chunks <- chunkOf(chunk100ms, 2)
		expectUtterance(t, l)
	}
	if got := j.Calls(); got != 2 {
		t.Errorf("judge called %d times, want 2 (one per utterance)", got)
	}
	n := 0
	for _, w := range sink.Warnings() {
		if strings.Contains(w, "boom") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("judge-error warnings = %d (%v), want exactly 1", n, sink.Warnings())
	}
}

func TestTurnJudgeUserMuteDuringHoldFinalizes(t *testing.T) {
	v := &fakeVAD{script: append([]fakeVADStep{{started: true}, {ended: true}}, silentSteps(40)...)}
	j := &fakeJudge{probs: []float32{0.2}}
	chunks, l := newJudgedListener(t, v, j)
	_, aborts := speechHookCounts(l)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	if !waitUntil(time.Second, func() bool { return j.Calls() == 1 }) {
		t.Fatal("judge never asked")
	}
	l.SetMuted(true, true)
	u := expectUtterance(t, l)
	if len(u) != 2*chunk100ms {
		t.Errorf("finalized utterance = %d bytes, want %d", len(u), 2*chunk100ms)
	}
	if got := l.LastEndpointWait(); got != 0 {
		t.Errorf("LastEndpointWait after mute flush = %v, want 0", got)
	}
	if got := aborts.Load(); got != 0 {
		t.Errorf("OnSpeechAbort fired %d times, want 0", got)
	}

	// The hold is over: post-unmute silence must neither deliver anything
	// nor ask the judge again.
	l.SetMuted(false, false)
	for range 30 {
		chunks <- chunkOf(chunk100ms, 0)
	}
	expectNoUtterance(t, l, "after the hold was finalized by mute")
	if got := j.Calls(); got != 1 {
		t.Errorf("judge called %d times, want 1", got)
	}
}

func TestTurnJudgeHarnessMuteDuringHoldDiscards(t *testing.T) {
	v := &fakeVAD{script: append([]fakeVADStep{{started: true}, {ended: true}}, silentSteps(40)...)}
	j := &fakeJudge{probs: []float32{0.2}}
	chunks, l := newJudgedListener(t, v, j)
	_, aborts := speechHookCounts(l)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	if !waitUntil(time.Second, func() bool { return j.Calls() == 1 }) {
		t.Fatal("judge never asked")
	}
	l.SetMuted(true, false)
	if got := aborts.Load(); got != 1 {
		t.Errorf("OnSpeechAbort fired %d times, want 1", got)
	}
	expectNoUtterance(t, l, "by a harness mute")

	l.SetMuted(false, false)
	for range 30 { // well past max silence
		chunks <- chunkOf(chunk100ms, 0)
	}
	expectNoUtterance(t, l, "after the held utterance was discarded")
	if got := j.Calls(); got != 1 {
		t.Errorf("judge called %d times, want 1", got)
	}
}

// fedCount reports how many chunks the fake detector has consumed, read
// under l.mu (Feed runs under it) so it's race-free against Run.
func fedCount(l *Listener, v *fakeVAD) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return v.idx
}

func TestOnSustainedSpeechFiresOnceAfterThreshold(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{}, {started: true}, {}, {}, {}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SustainedSpeechMS = 300
	var fired atomic.Int32
	l.OnSustainedSpeech = func() { fired.Add(1) }
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 0) // pre-speech: preBuf only, never counts
	chunks <- chunkOf(chunk100ms, 1) // started: 100 ms
	chunks <- chunkOf(chunk100ms, 2) // 200 ms
	if !waitUntil(time.Second, func() bool { return fedCount(l, v) == 3 }) {
		t.Fatal("test setup: chunks never processed")
	}
	time.Sleep(20 * time.Millisecond)
	if got := fired.Load(); got != 0 {
		t.Fatalf("OnSustainedSpeech fired %d times at 200 ms of speech, want 0", got)
	}
	chunks <- chunkOf(chunk100ms, 3) // 300 ms: threshold reached
	if !waitUntil(time.Second, func() bool { return fired.Load() == 1 }) {
		t.Fatalf("OnSustainedSpeech fired %d times at 300 ms of speech, want 1", fired.Load())
	}
	chunks <- chunkOf(chunk100ms, 4) // 400 ms: still once
	chunks <- chunkOf(chunk100ms, 5) // ended
	expectUtterance(t, l)
	if got := fired.Load(); got != 1 {
		t.Errorf("OnSustainedSpeech fired %d times over one utterance, want 1", got)
	}
}

func TestOnSustainedSpeechNotForShortUtterance(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SustainedSpeechMS = 300
	var fired atomic.Int32
	l.OnSustainedSpeech = func() { fired.Add(1) }
	go l.Run()
	defer close(chunks)

	for i := range 3 {
		chunks <- chunkOf(chunk100ms, byte(i))
	}
	expectUtterance(t, l)
	if got := fired.Load(); got != 0 {
		t.Errorf("OnSustainedSpeech fired %d times for 200 ms of speech, want 0", got)
	}
}

func TestDropPendingDeliveryDiscardsBlockedUtterance(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {ended: true}, {started: true}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2) // ended: Run blocks delivering, nobody receiving
	if !waitUntil(time.Second, func() bool { return fedCount(l, v) == 2 }) {
		t.Fatal("test setup: chunks never processed")
	}
	if !l.DropPendingDelivery() {
		t.Fatal("DropPendingDelivery() = false with an undelivered utterance pending")
	}
	expectNoUtterance(t, l, "after DropPendingDelivery discarded it")
	if l.DropPendingDelivery() {
		t.Error("second DropPendingDelivery() = true, want false (already dropped)")
	}

	// The listener keeps working: the next utterance is delivered normally.
	chunks <- chunkOf(4, 3)
	chunks <- chunkOf(4, 4)
	if got := expectUtterance(t, l); string(got) != string(append(chunkOf(4, 3), chunkOf(4, 4)...)) {
		t.Errorf("next utterance = %v, want just the second utterance's chunks", got)
	}
}

func TestDropPendingDeliveryNoopWhenNothingPending(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()
	defer close(chunks)

	if l.DropPendingDelivery() {
		t.Error("DropPendingDelivery() = true on an idle listener, want false")
	}
	chunks <- chunkOf(4, 1)
	chunks <- chunkOf(4, 2)
	expectUtterance(t, l)
	if l.DropPendingDelivery() {
		t.Error("DropPendingDelivery() = true after the utterance was received, want false")
	}
}

// TestDropPendingDeliveryRaceWithReceiverIsTruthful races a receiver
// against DropPendingDelivery for the same blocked utterance: exactly one
// of them must win -- never both (a "dropped" utterance that was actually
// received) and never neither (a lost utterance).
func TestDropPendingDeliveryRaceWithReceiverIsTruthful(t *testing.T) {
	for i := range 50 {
		v := &fakeVAD{script: []fakeVADStep{{started: true, ended: true}}}
		chunks := make(chan []byte)
		l := NewListener(chunks, v, 500)
		go l.Run()

		chunks <- chunkOf(4, 1)
		if !waitUntil(time.Second, func() bool { return fedCount(l, v) == 1 }) {
			t.Fatal("test setup: chunk never processed")
		}
		received := make(chan bool, 1)
		go func() {
			select {
			case <-l.Utterances():
				received <- true
			case <-time.After(50 * time.Millisecond):
				received <- false
			}
		}()
		dropped := l.DropPendingDelivery()
		got := <-received
		if dropped == got {
			t.Fatalf("trial %d: dropped=%v received=%v, want exactly one", i, dropped, got)
		}
		close(chunks)
	}
}

// TestOnSustainedSpeechCountsDetectorSilenceDebounce pins the documented
// semantics: the count is time in the detector's speaking state, so an
// onset followed by pure silence the detector is still debouncing (still
// Speaking, not yet ended) fires the hook once the threshold is reached --
// it is not a real speech-duration measure.
func TestOnSustainedSpeechCountsDetectorSilenceDebounce(t *testing.T) {
	// fakeVAD keeps reporting Speaking after a started step until an ended
	// step, exactly like a real detector waiting out its silence.
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {}, {}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SustainedSpeechMS = 300
	var fired atomic.Int32
	l.OnSustainedSpeech = func() { fired.Add(1) }
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1) // onset (e.g. a cough)
	chunks <- chunkOf(chunk100ms, 0) // silence, detector still speaking
	chunks <- chunkOf(chunk100ms, 0) // silence: 300 ms in speaking state
	if !waitUntil(time.Second, func() bool { return fired.Load() == 1 }) {
		t.Fatalf("OnSustainedSpeech fired %d times after onset + 200 ms debounce silence, want 1", fired.Load())
	}
	chunks <- chunkOf(chunk100ms, 0) // ended
	expectUtterance(t, l)
	if got := fired.Load(); got != 1 {
		t.Errorf("OnSustainedSpeech fired %d times, want 1", got)
	}
}

// noMeterVAD hides fakeVAD's VoicedMS, standing in for a detector that
// can't measure speech (the remote one).
type noMeterVAD struct{ VoiceDetector }

func TestMinSpeechDropsShortUtteranceAndAborts(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{
		{voiced: 100}, {started: true, voiced: 100}, {}, {ended: true}, // 200 ms of speech: dropped
		{voiced: 100}, {started: true, voiced: 100}, {voiced: 100}, {ended: true}, // 300 ms: delivered
	}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SetMinSpeechMS(300)
	var aborts atomic.Int32
	l.OnSpeechAbort = func() { aborts.Add(1) }
	go l.Run()
	defer close(chunks)

	for i := range 4 {
		chunks <- chunkOf(chunk100ms, byte(i+1))
	}
	expectNoUtterance(t, l, "for 200 ms of speech with a 300 ms minimum")
	if got := aborts.Load(); got != 1 {
		t.Errorf("OnSpeechAbort fired %d times for a dropped utterance, want 1", got)
	}
	for i := range 4 {
		chunks <- chunkOf(chunk100ms, byte(i+5))
	}
	if u := expectUtterance(t, l); len(u) != 4*chunk100ms {
		t.Errorf("utterance = %d bytes, want %d (pre-buffer included)", len(u), 4*chunk100ms)
	}
	if got := aborts.Load(); got != 1 {
		t.Errorf("OnSpeechAbort fired %d times in total, want 1", got)
	}
}

// Speech that has already scrolled out of the pre-buffer isn't in the
// utterance, so it doesn't count toward the minimum.
func TestMinSpeechIgnoresTrimmedPreBuffer(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{
		{voiced: 100}, {voiced: 100}, {voiced: 100}, {started: true}, {ended: true},
	}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 200) // keeps the last two 100 ms chunks
	l.SetMinSpeechMS(300)
	go l.Run()
	defer close(chunks)

	for i := range 5 {
		chunks <- chunkOf(chunk100ms, byte(i+1))
	}
	expectNoUtterance(t, l, "with only 200 ms of speech left in the pre-buffer")
}

func TestMinSpeechAppliesToJudgedDelivery(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{
		{started: true, voiced: 100}, {ended: true}, // judged done, but 100 ms
		{started: true, voiced: 100}, {voiced: 100}, {voiced: 100}, {ended: true},
	}}
	j := &fakeJudge{probs: []float32{0.9}}
	chunks, l := newJudgedListener(t, v, j)
	l.SetMinSpeechMS(250)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	expectNoUtterance(t, l, "for 100 ms of speech")
	for i := range 4 {
		chunks <- chunkOf(chunk100ms, byte(i+3))
	}
	if u := expectUtterance(t, l); len(u) != 4*chunk100ms {
		t.Errorf("utterance = %d bytes, want %d (the dropped one must not leak into it)", len(u), 4*chunk100ms)
	}
}

func TestMinSpeechCountsResumedSpeechInHold(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{
		{started: true, voiced: 100}, {ended: true}, // judged unfinished
		{started: true, voiced: 100}, {voiced: 100}, {ended: true}, // judged done: 300 ms in all
	}}
	j := &fakeJudge{probs: []float32{0.1, 0.9}}
	chunks, l := newJudgedListener(t, v, j)
	l.SetMinSpeechMS(300)
	go l.Run()
	defer close(chunks)

	for i := range 5 {
		chunks <- chunkOf(chunk100ms, byte(i+1))
	}
	if u := expectUtterance(t, l); len(u) != 5*chunk100ms {
		t.Errorf("utterance = %d bytes, want %d", len(u), 5*chunk100ms)
	}
}

func TestMinSpeechAppliesToMaxSilenceDelivery(t *testing.T) {
	steps := []fakeVADStep{{started: true, voiced: 100}, {ended: true}}
	steps = append(steps, silentSteps(30)...)
	v := &fakeVAD{script: steps}
	j := &fakeJudge{probs: []float32{0.1}} // never done: the cap delivers
	chunks, l := newJudgedListener(t, v, j)
	l.SetMinSpeechMS(300)
	go l.Run()
	defer close(chunks)

	for i := range 32 {
		chunks <- chunkOf(chunk100ms, byte(i))
	}
	expectNoUtterance(t, l, "at the quiet cap for 100 ms of speech")
}

func TestMinSpeechKeepsMuteFinalizedUtterance(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true, voiced: 100}, {}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	l.SetMinSpeechMS(300)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	if !waitUntil(time.Second, func() bool { return fedCount(l, v) == 2 }) {
		t.Fatal("test setup: chunks never processed")
	}
	l.SetMuted(true, true)
	if u := expectUtterance(t, l); len(u) != 2*chunk100ms {
		t.Errorf("mute-finalized utterance = %d bytes, want %d", len(u), 2*chunk100ms)
	}
}

func TestMinSpeechIgnoredWithoutMeter(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, noMeterVAD{v}, 500)
	if l.SetMinSpeechMS(300) {
		t.Error("SetMinSpeechMS reported active for a detector without VoicedMS")
	}
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	expectUtterance(t, l)
}

// TestBusyDuringHold: a paused utterance held for the turn judge reads as
// not speaking but busy, and LastSpeech moves with it.
func TestBusyDuringHold(t *testing.T) {
	v := &fakeVAD{script: append([]fakeVADStep{{started: true}, {ended: true}}, silentSteps(40)...)}
	j := &fakeJudge{probs: []float32{0.2}}
	chunks, l := newJudgedListener(t, v, j)
	go l.Run()
	defer close(chunks)

	if l.Busy() || !l.LastSpeech().IsZero() {
		t.Fatalf("Busy/LastSpeech = %v/%v before any speech", l.Busy(), l.LastSpeech())
	}
	before := time.Now()
	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	if !waitUntil(time.Second, func() bool { return j.Calls() == 1 }) {
		t.Fatal("judge never asked")
	}
	if l.Speaking() {
		t.Error("Speaking() = true during the hold, want false")
	}
	if !l.Busy() {
		t.Error("Busy() = false during the hold, want true")
	}
	if l.LastSpeech().Before(before) {
		t.Errorf("LastSpeech = %v, want after %v", l.LastSpeech(), before)
	}
}

// TestBusyWhileDeliveryPending: an ended utterance nobody has received yet
// keeps the listener busy until it is received.
func TestBusyWhileDeliveryPending(t *testing.T) {
	v := &fakeVAD{script: []fakeVADStep{{started: true}, {ended: true}}}
	chunks := make(chan []byte)
	l := NewListener(chunks, v, 500)
	go l.Run()
	defer close(chunks)

	chunks <- chunkOf(chunk100ms, 1)
	chunks <- chunkOf(chunk100ms, 2)
	if !waitUntil(time.Second, func() bool { return fedCount(l, v) == 2 }) {
		t.Fatal("chunks not fed")
	}
	if l.Speaking() {
		t.Error("Speaking() = true after the utterance ended")
	}
	if !l.Busy() {
		t.Error("Busy() = false with an undelivered utterance, want true")
	}
	expectUtterance(t, l)
	if !waitUntil(time.Second, func() bool { return !l.Busy() }) {
		t.Error("Busy() still true after the utterance was received")
	}
}
