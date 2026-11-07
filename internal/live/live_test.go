package live

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nlm/duetty/internal/convo"
	"github.com/nlm/duetty/internal/stt"
)

var _ Sink = (*convo.Log)(nil)

// fakeResolver records each call's language and audio size and answers with
// respond(callIndex). With a gate, each call waits for the gate to close (or
// for ctx, unless ignoreCtx).
type fakeResolver struct {
	mu        sync.Mutex
	langs     []string
	sizes     []int
	respond   func(i int) (stt.Resolved, error)
	gate      chan struct{}
	ignoreCtx bool
	called    chan struct{}
}

func newResolver(respond func(i int) (stt.Resolved, error)) *fakeResolver {
	return &fakeResolver{respond: respond, called: make(chan struct{}, 1000)}
}

func (r *fakeResolver) ResolveLocked(ctx context.Context, wav []byte, lang string) (stt.Resolved, error) {
	r.mu.Lock()
	i := len(r.langs)
	r.langs = append(r.langs, lang)
	r.sizes = append(r.sizes, len(wav))
	r.mu.Unlock()
	r.called <- struct{}{}
	if r.gate != nil {
		if r.ignoreCtx {
			<-r.gate
		} else {
			select {
			case <-r.gate:
			case <-ctx.Done():
				return stt.Resolved{}, ctx.Err()
			}
		}
	}
	return r.respond(i)
}

func (r *fakeResolver) calls() ([]string, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.langs...), append([]int(nil), r.sizes...)
}

func (r *fakeResolver) waitCalled(t *testing.T) {
	t.Helper()
	select {
	case <-r.called:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver not called")
	}
}

type fakeSink struct {
	mu    sync.Mutex
	next  convo.LiveID
	calls []string
}

func (s *fakeSink) record(format string, args ...any) {
	s.mu.Lock()
	s.calls = append(s.calls, fmt.Sprintf(format, args...))
	s.mu.Unlock()
}

func (s *fakeSink) StartLive() convo.LiveID {
	s.mu.Lock()
	s.next++
	id := s.next
	s.calls = append(s.calls, fmt.Sprintf("start %d", id))
	s.mu.Unlock()
	return id
}
func (s *fakeSink) UpdateLive(id convo.LiveID, lang, text string) {
	s.record("update %d %s %s", id, lang, text)
}
func (s *fakeSink) CommitLive(id convo.LiveID, lang, text string) {
	s.record("commit %d %s %s", id, lang, text)
}
func (s *fakeSink) DropLive(id convo.LiveID) { s.record("drop %d", id) }

func (s *fakeSink) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// identity encoding: the resolver sees the 48 kHz snapshot itself.
func testConfig() Config {
	return Config{
		IntervalMS: 1000,
		MinMS:      300,
		LockAfter:  3 * time.Second,
		Timeout:    5 * time.Second,
		Encode:     func(pcm []byte) ([]byte, error) { return append([]byte(nil), pcm...), nil },
	}
}

// audio returns d seconds of 48 kHz s16le audio filled with b.
func audio(d float64, b byte) []byte {
	return bytes.Repeat([]byte{b}, bytesAt(d))
}

func text(lang, s string, segs ...stt.Segment) stt.Resolved {
	return stt.Resolved{Lang: lang, Text: s, Segments: segs}
}

// feed sends d seconds in 100 ms chunks, waiting for refreshes after each.
func feed(p *Previewer, d float64, start bool) {
	for i := 0; i < int(d*10+0.5); i++ {
		p.Audio(audio(0.1, 0), start && i == 0)
		p.wg.Wait()
	}
}

func TestRefreshAndCommit(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) {
		return text("fr", "Bonjour. Je", stt.Segment{Start: 0, End: 1.2, Text: "Bonjour."},
			stt.Segment{Start: 1.5, End: 2.0, Text: "Je"}), nil
	})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	all := audio(2, 0)
	p.Audio(all, true)
	p.wg.Wait()
	langs, sizes := r.calls()
	if len(langs) != 1 || langs[0] != "" || sizes[0] != len(all) {
		t.Fatalf("calls: langs %q sizes %v", langs, sizes)
	}
	want := []string{"start 1", "commit 1 fr Bonjour.", "update 1 fr Je"}
	if got := s.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sink %q, want %q", got, want)
	}
	rest, id, lang := p.End(all)
	if id != 1 || lang != "" || len(rest) != len(all)-bytesAt(1.2) {
		t.Fatalf("End: id %d lang %q rest %d", id, lang, len(rest))
	}
	p.Close()
}

func TestOneRefreshInFlight(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) { return text("fr", "x"), nil })
	r.gate = make(chan struct{})
	p := New(testConfig(), r, &fakeSink{})
	for i := 0; i < 50; i++ {
		p.Audio(audio(0.1, 0), i == 0)
		if i == 10 {
			r.waitCalled(t)
		}
	}
	if langs, _ := r.calls(); len(langs) != 1 {
		t.Fatalf("%d calls while blocked, want 1", len(langs))
	}
	close(r.gate)
	p.Close()
	if langs, _ := r.calls(); len(langs) != 1 {
		t.Fatalf("%d calls after release, want 1", len(langs))
	}
}

func TestPacingSlowsForLongRecording(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) { return text("fr", "blah blah"), nil })
	p := New(testConfig(), r, &fakeSink{})
	feed(p, 20, true)
	_, sizes := r.calls()
	total := 0
	for _, n := range sizes {
		total += n
	}
	t.Logf("%d refreshes, %.1f s of audio sent", len(sizes), float64(total)/bytesPerSec)
	if len(sizes) > 12 || len(sizes) < 5 {
		t.Fatalf("%d refreshes over 20 s, want 5..12", len(sizes))
	}
	if total > 5*bytesAt(20) {
		t.Fatalf("sent %.1f s of audio for a 20 s recording, want ≤ 100 s", float64(total)/bytesPerSec)
	}
	p.Close()
}

func TestLanguageLocksAfter3s(t *testing.T) {
	r := newResolver(func(i int) (stt.Resolved, error) {
		if i == 3 { // the 4 s refresh commits the first 2 s
			return text("fr", "Un. Deux", stt.Segment{Start: 0, End: 2, Text: "Un."},
				stt.Segment{Start: 2, End: 4, Text: "Deux"}), nil
		}
		return text("fr", "texte"), nil
	})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	feed(p, 4.5, true)
	p.mu.Lock()
	unlocked := p.turns[0].locked == ""
	p.mu.Unlock()
	if !unlocked {
		t.Fatal("language still locked after a commit")
	}
	feed(p, 0.5, false)
	langs, sizes := r.calls()
	wantLangs := []string{"", "", "", "fr", ""}
	wantSizes := []int{bytesAt(1), bytesAt(2), bytesAt(3), bytesAt(4), bytesAt(3)}
	if !reflect.DeepEqual(langs, wantLangs) || !reflect.DeepEqual(sizes, wantSizes) {
		t.Fatalf("langs %q sizes %v, want %q %v", langs, sizes, wantLangs, wantSizes)
	}
	// The 5 s refresh's 3 s snapshot locks again.
	if _, _, lang := p.End(audio(5, 0)); lang != "fr" {
		t.Fatalf("End lang %q, want fr", lang)
	}
	p.Close()
}

func TestLockedLanguageReturnedByEnd(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) { return text("ja", "駅は"), nil })
	p := New(testConfig(), r, &fakeSink{})
	feed(p, 3.5, true)
	if _, _, lang := p.End(audio(3.5, 0)); lang != "ja" {
		t.Fatalf("End lang %q, want ja", lang)
	}
	p.Close()
}

func TestShownLanguageStaysUntilLock(t *testing.T) {
	r := newResolver(func(i int) (stt.Resolved, error) {
		if i == 1 {
			return text("en", "two"), nil
		}
		return text("fr", "un"), nil
	})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	feed(p, 3, true)
	want := []string{"start 1", "update 1 fr un", "update 1 fr two", "update 1 fr un"}
	if got := s.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sink %q, want %q", got, want)
	}
	p.Close()
}

func TestStaleRefreshIgnored(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) {
		return text("fr", "Un. Deux", stt.Segment{Start: 0, End: 1, Text: "Un."},
			stt.Segment{Start: 1, End: 2, Text: "Deux"}), nil
	})
	r.gate = make(chan struct{})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	all := audio(2, 0)
	p.Audio(all, true)
	r.waitCalled(t)
	rest, id, _ := p.End(all)
	if id != 1 || len(rest) != len(all) {
		t.Fatalf("End: id %d rest %d", id, len(rest))
	}
	before := s.log()
	close(r.gate)
	p.Close()
	if got := s.log(); !reflect.DeepEqual(got, before) {
		t.Fatalf("sink calls after End: %q", got[len(before):])
	}
}

func TestCommitTrimClamped(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) {
		return text("fr", "Un. Deux", stt.Segment{Start: 0, End: 9.9, Text: "Un."},
			stt.Segment{Start: 9.9, End: 10, Text: "Deux"}), nil
	})
	p := New(testConfig(), r, &fakeSink{})
	first := audio(2, 0)
	tail := audio(0.5, 7)
	p.Audio(first, true)
	p.wg.Wait()
	p.Audio(tail, false)
	p.wg.Wait()
	rest, _, _ := p.End(append(append([]byte(nil), first...), tail...))
	if !bytes.Equal(rest, tail) {
		t.Fatalf("rest %d bytes, want the %d bytes after the snapshot", len(rest), len(tail))
	}
	p.Close()
}

func TestEndPairsWithOldestTurn(t *testing.T) {
	r := newResolver(func(i int) (stt.Resolved, error) {
		if i == 0 {
			return text("fr", "Un. Deux", stt.Segment{Start: 0, End: 1, Text: "Un."},
				stt.Segment{Start: 1, End: 2, Text: "Deux"}), nil
		}
		return text("en", "b"), nil
	})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	a := audio(2, 1)
	p.Audio(a, true)
	p.wg.Wait()
	p.Audio(audio(1, 2), true) // turn B starts before A is delivered
	p.wg.Wait()
	rest, id, _ := p.End(a)
	if id != 1 || !bytes.Equal(rest, a[bytesAt(1):]) {
		t.Fatalf("End(A): id %d rest %d bytes", id, len(rest))
	}
	p.Audio(audio(1, 2), false)
	p.wg.Wait()
	_, sizes := r.calls()
	if len(sizes) != 3 || sizes[1] != bytesAt(1) || sizes[2] != bytesAt(2) {
		t.Fatalf("refresh sizes %v, want A then B at 1 s and 2 s", sizes)
	}
	want := []string{"start 1", "commit 1 fr Un.", "update 1 fr Deux", "start 2", "update 2 en b", "update 2 en b"}
	if got := s.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sink %q, want %q", got, want)
	}
	if _, id, _ := p.End(audio(2, 2)); id != 2 {
		t.Fatalf("End(B) id %d, want 2", id)
	}
	p.Close()
}

func TestAbortDropsNewest(t *testing.T) {
	r := newResolver(func(int) (stt.Resolved, error) { return text("fr", "x"), nil })
	r.gate = make(chan struct{})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	p.Audio(audio(2, 0), true)
	r.waitCalled(t)
	p.Abort()
	close(r.gate)
	p.wg.Wait()
	want := []string{"start 1", "drop 1"}
	if got := s.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sink %q, want %q", got, want)
	}
	p.Audio(audio(0.5, 0), false) // stray audio after abort: no turn
	p.Audio(audio(0.5, 0), true)
	if _, id, _ := p.End(audio(0.5, 0)); id != 2 {
		t.Fatalf("End after abort paired with %d, want 2", id)
	}
	p.Close()
}

func TestAbortIgnoresEndedTurn(t *testing.T) {
	s := &fakeSink{}
	p := New(testConfig(), newResolver(func(int) (stt.Resolved, error) { return text("fr", "x"), nil }), s)
	p.Audio(audio(0.2, 0), true)
	p.End(audio(0.2, 0))
	p.Abort()
	if got := s.log(); !reflect.DeepEqual(got, []string{"start 1"}) {
		t.Fatalf("sink %q", got)
	}
	p.Close()
}

func TestNoiseAndErrorsShowNothing(t *testing.T) {
	r := newResolver(func(i int) (stt.Resolved, error) {
		if i == 0 {
			return stt.Resolved{}, stt.ErrNoise
		}
		return stt.Resolved{}, fmt.Errorf("boom")
	})
	s := &fakeSink{}
	p := New(testConfig(), r, s)
	feed(p, 2, true)
	if langs, _ := r.calls(); len(langs) != 2 {
		t.Fatalf("%d calls, want 2", len(langs))
	}
	if got := s.log(); !reflect.DeepEqual(got, []string{"start 1"}) {
		t.Fatalf("sink %q", got)
	}
	p.Close()
}

func TestShortRecordingNotSent(t *testing.T) {
	cfg := testConfig()
	cfg.IntervalMS = 100
	cfg.MinMS = 500
	r := newResolver(func(int) (stt.Resolved, error) { return text("fr", "x"), nil })
	p := New(cfg, r, &fakeSink{})
	feed(p, 0.4, true)
	if langs, _ := r.calls(); len(langs) != 0 {
		t.Fatalf("%d calls below MinMS, want 0", len(langs))
	}
	p.Close()
}

func TestCloseBoundedByTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	r := newResolver(func(int) (stt.Resolved, error) { return text("fr", "x"), nil })
	r.gate = make(chan struct{})
	r.ignoreCtx = true
	defer close(r.gate)
	p := New(cfg, r, &fakeSink{})
	p.Audio(audio(2, 0), true)
	r.waitCalled(t)
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within its bound")
	}
}

// ctxResolver blocks every call until its context is done and records the
// context's error.
type ctxResolver struct {
	called chan struct{}
	errs   chan error
}

func (r *ctxResolver) ResolveLocked(ctx context.Context, _ []byte, _ string) (stt.Resolved, error) {
	r.called <- struct{}{}
	<-ctx.Done()
	r.errs <- ctx.Err()
	return stt.Resolved{}, ctx.Err()
}

func TestCloseCancelsInFlightRefresh(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 30 * time.Second
	r := &ctxResolver{called: make(chan struct{}, 10), errs: make(chan error, 10)}
	p := New(cfg, r, &fakeSink{})
	p.Audio(audio(2, 0), true)
	select {
	case <-r.called:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver not called")
	}
	start := time.Now()
	p.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v with a blocked refresh, want well under Timeout", d)
	}
	select {
	case err := <-r.errs:
		if err != context.Canceled {
			t.Fatalf("resolver saw %v, want context.Canceled", err)
		}
	default:
		t.Fatal("resolver did not see cancellation")
	}
}
