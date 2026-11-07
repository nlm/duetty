// Package live re-transcribes the turn being spoken while it is recorded,
// shows the result provisionally and commits finished sentences early.
package live

import (
	"context"
	"sync"
	"time"

	"github.com/nlm/poly/internal/convo"
	"github.com/nlm/poly/internal/stt"
)

// Resolver transcribes 16 kHz WAV audio; lang "" auto-detects, otherwise
// the language is forced.
type Resolver interface {
	ResolveLocked(ctx context.Context, wav16k []byte, lang string) (stt.Resolved, error)
}

// Sink receives the provisional and committed text of live turns.
// *convo.Log satisfies it.
type Sink interface {
	StartLive() convo.LiveID
	UpdateLive(id convo.LiveID, lang, text string)
	CommitLive(id convo.LiveID, lang, text string)
	DropLive(id convo.LiveID)
}

// Config sets the previewer's pacing and language lock.
type Config struct {
	IntervalMS float64       // minimum new audio between refreshes
	MinMS      float64       // live recordings shorter than this are not sent
	LockAfter  time.Duration // snapshot length from which the language locks
	Timeout    time.Duration // bounds each refresh, and Close's wait
	Encode     func(pcm48k []byte) ([]byte, error)
}

// turn is one utterance being captured (or captured, awaiting End).
type turn struct {
	id       convo.LiveID
	buf      []byte // all audio of the utterance so far
	base     int    // committed offset into buf: the live recording is buf[base:]
	sent     int    // len(buf) at the last refresh sent
	inFlight bool
	sealed   bool // a newer turn started (or End was called): no more refreshes
	done     bool // ended or aborted: results are ignored
	locked   string
	shown    string
	hasShown bool // shown was set since the turn start or the last commit
}

// Previewer paces refreshes of the live recording and turns their results
// into Sink calls. Sink methods are called with the previewer's lock held,
// so no call for a turn happens after End or Abort returned for it.
type Previewer struct {
	cfg  Config
	r    Resolver
	sink Sink

	ctx    context.Context // parent of every refresh; cancelled by Close
	cancel context.CancelFunc

	mu     sync.Mutex
	turns  []*turn // turns not yet ended or aborted, oldest first
	closed bool
	wg     sync.WaitGroup
}

// New returns a Previewer. A nil cfg.Encode means stt.PCM48kToWAV16k.
func New(cfg Config, r Resolver, s Sink) *Previewer {
	if cfg.Encode == nil {
		cfg.Encode = stt.PCM48kToWAV16k
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Previewer{cfg: cfg, r: r, sink: s, ctx: ctx, cancel: cancel}
}

// newest returns the turn receiving audio, or nil.
// Caller holds p.mu.
func (p *Previewer) newest() *turn {
	if n := len(p.turns); n > 0 && !p.turns[n-1].sealed {
		return p.turns[n-1]
	}
	return nil
}

// Audio appends data (copied) to the turn being captured; start begins a new
// turn. It may start a refresh in the background and never waits for one.
func (p *Previewer) Audio(data []byte, start bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if start {
		if t := p.newest(); t != nil {
			t.sealed = true
		}
		p.turns = append(p.turns, &turn{id: p.sink.StartLive()})
	}
	t := p.newest()
	if t == nil {
		return // audio of an aborted utterance
	}
	t.buf = append(t.buf, data...)
	if t.inFlight {
		return
	}
	liveMS := msOf(len(t.buf) - t.base)
	if liveMS < p.cfg.MinMS || !Due(msOf(len(t.buf)-t.sent), liveMS, p.cfg.IntervalMS) {
		return
	}
	t.inFlight = true
	t.sent = len(t.buf)
	// buf is only ever appended to, so this window is never written again.
	snap := t.buf[t.base:len(t.buf):len(t.buf)]
	lang := t.locked
	p.wg.Add(1)
	go p.refresh(t, snap, t.base, lang)
}

// refresh transcribes one snapshot of t's live recording, taken at base.
func (p *Previewer) refresh(t *turn, snap []byte, base int, lang string) {
	defer p.wg.Done()
	var res stt.Resolved
	wav, err := p.cfg.Encode(snap)
	if err == nil {
		ctx, cancel := context.WithTimeout(p.ctx, p.cfg.Timeout)
		res, err = p.r.ResolveLocked(ctx, wav, lang)
		cancel()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.inFlight = false
	if t.done || t.base != base || err != nil {
		return // stale, noise or failed: show nothing
	}
	if !t.hasShown {
		t.shown, t.hasShown = res.Lang, true
	}
	if t.locked == "" && msOf(len(snap)) >= float64(p.cfg.LockAfter.Milliseconds()) {
		t.locked, t.shown = res.Lang, res.Lang
	}
	n := CommitCount(res.Segments)
	if n == 0 {
		p.sink.UpdateLive(t.id, t.shown, res.Text)
		return
	}
	p.sink.CommitLive(t.id, res.Lang, join(res.Segments[:n]))
	t.base += min(max(bytesAt(res.Segments[n-1].End), 0), len(snap))
	t.locked, t.hasShown = "", false
	p.sink.UpdateLive(t.id, t.shown, join(res.Segments[n:]))
}

// Abort drops the turn being captured, if any: its live entry is removed and
// its in-flight refresh ignored. Committed sentences stay.
func (p *Previewer) Abort() {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.newest()
	if t == nil {
		return
	}
	t.done = true
	p.turns = p.turns[:len(p.turns)-1]
	p.sink.DropLive(t.id)
}

// End pairs a delivered utterance with the oldest turn not yet ended and
// returns the part of utterance not yet committed, the turn's live id and
// its locked language ("" if unlocked). Later results for it are ignored.
// With no turn to pair with, it returns utterance whole and id 0.
func (p *Previewer) End(utterance []byte) (rest []byte, id convo.LiveID, lang string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) == 0 {
		return utterance, 0, ""
	}
	t := p.turns[0]
	p.turns = p.turns[1:]
	t.sealed, t.done = true, true
	return utterance[min(t.base, len(utterance)):], t.id, t.locked
}

// Close stops new refreshes, cancels in-flight ones and waits for them to
// unwind, at most Timeout.
func (p *Previewer) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(p.cfg.Timeout):
	}
}
