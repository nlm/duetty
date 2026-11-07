// Package vad drives an always-open microphone PCM stream through a
// voice-activity detector, delivering whole captured utterances, and
// provides the mic-level calibration helpers behind the mic-levels
// subcommand.
package vad

import (
	"sync"
	"time"

	"github.com/nlm/poly/internal/audiolevel"
	"github.com/nlm/poly/internal/audioutil"
	"github.com/nlm/poly/internal/log"
)

// SampleRate is the PCM sample rate (Hz) this package assumes for incoming
// chunks -- 48000Hz mono s16le, matching internal/audioio.SampleRate. Kept
// as its own constant (rather than importing audioio) so this package stays
// decoupled from any concrete mic implementation, per the plain <-chan
// []byte contract below.
const SampleRate = 48000

// VoiceDetector is whatever the Listener drives incoming PCM chunks
// through to detect utterance boundaries -- either the default energy-based
// *audiolevel.VAD or the opt-in *silerovad.VAD. DetectionLabel returns a
// short, engine-specific diagnostic string used only by the state-logging
// "speech detected" line. Reset forces the detector back to "not
// speaking," discarding any in-progress onset/silence debounce
// accumulation but leaving calibration state (e.g. the energy VAD's
// learned noise floor) untouched -- used by SetMuted whenever muting lands
// mid-utterance, whether or not the partial capture ends up finalized (see
// SetMuted's doc comment).
type VoiceDetector interface {
	Feed(pcm []byte) (started, ended bool)
	Speaking() bool
	DetectionLabel() string
	Reset()
}

// SpeechMeter is an optional VoiceDetector extension: VoicedMS reports how
// many milliseconds of the audio passed to the most recent Feed call the
// detector classified as speech. It's what lets the Listener drop an
// utterance with too little actual speech in it (see SetMinSpeechMS) --
// a detector stays "speaking" through its whole silence wait, so time
// spent in that state can't tell a cough from a short word. Both local
// detectors implement it; the remote one doesn't (its wire protocol has
// no such field), which leaves the minimum off.
type SpeechMeter interface {
	VoicedMS() float64
}

// Listener drives an always-open microphone stream through a
// VoiceDetector, buffering each detected utterance and delivering it whole
// on Utterances. While muted, incoming audio is discarded entirely rather
// than fed to the vad -- used to stop the app from "hearing" its own
// synthesized replies coming back through the microphone (there's no
// acoustic echo cancellation in this path).
type Listener struct {
	chunks     <-chan []byte
	vad        VoiceDetector
	utterances chan []byte
	preBufMS   float64 // how much recent audio to keep rolling before an utterance is confirmed

	mu    sync.Mutex
	muted bool
	// silenceMS is the configured end-of-utterance silence; lastWait is the
	// endpointing wait of the most recently delivered utterance (see
	// LastEndpointWait). Both guarded by mu.
	silenceMS float64
	lastWait  time.Duration
	buf       []byte
	preBuf    []byte

	// pendingFlush and flushSignal hand a mid-utterance finalized
	// utterance (see SetMuted) from whatever arbitrary goroutine called
	// SetMuted over to Run's own goroutine for delivery, so Run remains
	// the *only* goroutine that ever sends on or closes utterances --
	// this is what makes a flush delivery race a concurrent close (or
	// race another flush for delivery order) structurally impossible,
	// rather than something that needs its own extra synchronization to
	// prevent. pendingFlush is an ordered queue (not just a single slot)
	// so two flushes queued in quick succession are still delivered in
	// that same order; flushSignal is a capacity-1 "wake up and check the
	// queue" hint, not a value channel -- a non-blocking send into it
	// coalesces harmlessly if one is already pending, since Run always
	// drains the real queue under mu rather than trusting the signal
	// count.
	//
	// closing guards a narrower, separate hazard the queue alone doesn't
	// solve: without it, Run's shutdown could observe pendingFlush empty
	// and proceed to close(utterances) while a SetMuted call that's
	// concurrently deciding to enqueue a flush lands *just after* that
	// observation -- the flush would be appended to a queue nothing will
	// ever drain again, silently losing an utterance the user actually
	// spoke (confirmed empirically during development: a version without
	// this guard lost the flush in roughly a quarter of trials under
	// concurrent load, with no data race and no panic -- a pure
	// logic/ordering bug). SetMuted checks closing before appending, and
	// Run's shutdown sets it in the very same lock hold as its final
	// (now-empty) check -- so whichever of the two wins the race for mu,
	// the other one correctly observes its consequence: either the flush
	// was already safely queued and Run's shutdown will drain it before
	// closing is ever set, or closing was already set and the flush is
	// deliberately (not silently) discarded instead of enqueued.
	pendingFlush [][]byte
	flushSignal  chan struct{}
	closing      bool

	// judge is the optional end-of-turn model (see SetTurnJudge); a nil
	// judge.Judge keeps the plain fixed-silence endpointing. holding is
	// true while a paused utterance is held open waiting for the judge
	// (see Run); quietMS is how much continuous non-speech the held
	// utterance has accumulated so far (starting at silenceMS, the pause
	// the detector already waited out before reporting ended), and
	// nextCheckMS is the quietMS at which the judge is asked again.
	// holdGen is bumped every time SetMuted ends a hold, so a judgment Run
	// computed outside mu for a hold that a concurrent mute has since
	// finalized or discarded is recognized as stale and dropped instead of
	// delivering (or re-holding) an utterance that no longer exists. All
	// guarded by mu. judgeWarn makes a failing judge warn once per
	// Listener rather than once per utterance -- a broken model fails the
	// same way every time, and the fail-open delivery already keeps the
	// conversation working.
	judge       TurnJudgeConfig
	holding     bool
	quietMS     float64
	nextCheckMS float64
	holdGen     uint64
	judgeWarn   sync.Once

	// speechMS is how long the detector has been in its speaking state
	// since the current utterance's onset, trailing silence debounce
	// included (see OnSustainedSpeech); sustainedFired makes that hook
	// fire at most once per utterance. Both reset at each new utterance
	// (not at a hold's resumed speech, which is the same utterance), guarded
	// by mu.
	speechMS       float64
	sustainedFired bool

	// minSpeechMS is the least speech (per SpeechMeter) a naturally-ended
	// utterance must hold to be delivered (see SetMinSpeechMS); 0 is off.
	// voicedMS is the current utterance's speech so far, pre-buffer
	// included; preVoiced tracks the speech in each chunk still held in
	// preBuf (its bytes and voiced ms, oldest first) so the speech in
	// audio that scrolled out of the pre-buffer stops counting. All guarded
	// by mu; preVoiced is only maintained while minSpeechMS is set.
	minSpeechMS float64
	voicedMS    float64
	preVoiced   []chunkVoice

	// pending is the naturally-ended utterance Run is currently trying to
	// hand to a receiver, nil when there is none (see DropPendingDelivery).
	// Set in the same mu hold that takes the utterance out of buf, so there
	// is no window in which an ended utterance exists but DropPendingDelivery
	// can't see it. Guarded by mu.
	pending *pendingDelivery

	// lastSpeech is when Run last handled a chunk belonging to an
	// utterance (speaking, onset, or held for the turn judge) -- see
	// LastSpeech. Guarded by mu.
	lastSpeech time.Time

	// OnLevel, if set, is called with each processed chunk's RMS level
	// (see internal/audiolevel.RMS) -- a UI can use this to drive a live
	// mic-level meter. nil is a valid no-op. Never called while muted
	// (muted chunks are discarded entirely, not fed to the vad either --
	// see SetMuted), so a meter driven by this naturally goes quiet while
	// muted rather than needing to be told separately. Set directly, like
	// speech.Synthesizer.OnSpeakStart, before calling Run. Called from
	// inside Run's per-chunk loop while l.mu is held (unlike OnMuteChange,
	// deliberately called outside the lock -- see its own doc comment) --
	// harmless today since this codebase's only implementation is a plain
	// UI callback with no way back into this Listener, but an
	// implementation that ever called IsMuted/Speaking/SetMuted on this
	// same Listener would self-deadlock (sync.Mutex isn't reentrant).
	OnLevel func(rms float64)

	// OnSpeechStart, if set, is called once each time the vad confirms an
	// utterance has started (where Run logs "speech detected") -- once per
	// utterance, not per onset: speech resuming during a turn-judge hold
	// continues the held utterance (see Run) and doesn't fire it again. Called
	// from Run's goroutine *outside* l.mu, so it may call back into this
	// Listener. Never called while muted (muted chunks never reach the
	// vad). Set before calling Run.
	OnSpeechStart func()

	// SustainedSpeechMS and OnSustainedSpeech are the barge-in trigger:
	// OnSustainedSpeech is called once per utterance, as soon as the time
	// the detector has spent in its speaking state since the confirmed
	// onset reaches SustainedSpeechMS. Counted per chunk, from the onset
	// chunk itself on, wherever the detector reports speaking (so the
	// rolling pre-buffer never counts, a turn-judge hold's quiet chunks
	// don't, and its resumed speech does, being the same utterance). This
	// is not a real speech-duration measure: a detector stays speaking
	// through its whole end-of-utterance silence wait (-vad-silence-ms), so
	// that trailing silence counts too, and with SustainedSpeechMS below
	// the detector's silence wait the hook fires for any confirmed onset
	// (a cough that passed onset debounce, say) followed by enough
	// silence -- it measures "onset + N ms still in the speaking state",
	// filtering only what the onset debounce itself already rejects. A
	// true speech-only duration would need a per-chunk activity signal from
	// VoiceDetector, deliberately not added here. 0 or nil turns it off.
	// Called from Run's goroutine outside l.mu (after OnSpeechStart for the
	// same chunk), so it may call back into this Listener. Never called
	// while muted (muted chunks never reach the vad). Set before calling
	// Run.
	SustainedSpeechMS float64
	OnSustainedSpeech func()

	// OnSpeechAbort, if set, is called when an utterance in progress is
	// discarded without being delivered: SetMuted(true, ...) while
	// speaking (or while a paused utterance is held for the turn judge)
	// that doesn't finalize it, or an ended one dropped for holding too
	// little speech (SetMinSpeechMS). A delivered utterance (ended
	// naturally, or finalized by a user mute) calls neither hook -- the
	// turn path takes over from there. Called outside l.mu: from SetMuted's
	// caller's goroutine, possibly while that caller holds its own locks
	// (see OnMuteChange's doc comment for the same caveat), or from Run's
	// for a too-short drop.
	OnSpeechAbort func()

	// OnMuteChange, if set, is called from every SetMuted call (not just
	// ones a user explicitly requested) with the new state -- SetMuted also
	// runs automatically around every turn (muting
	// the mic while talktome is speaking, since there's no echo
	// cancellation), so a UI that only updates its own display on an
	// explicit mute toggle goes stale the moment a turn starts. nil is a
	// valid no-op, and is what internal/app's composition root leaves this
	// today: its runtime drives its own TUI badge update directly from
	// whichever of its mute mutators just ran (see runtime.applyMuteLocked)
	// instead. Called synchronously from inside SetMuted, itself sometimes
	// called while a caller's own lock is held (see
	// runtime.applyMuteLocked's doc comment) -- if this is ever wired up
	// again, the callback must never call back into anything that could
	// need a lock SetMuted's caller might already be holding, or it will
	// deadlock (sync.Mutex isn't reentrant).
	OnMuteChange func(muted bool)
}

// NewListener returns a Listener reading PCM chunks from chunks, feeding
// them through v, and keeping preBufMS worth of audio rolling before an
// utterance start is confirmed (so the very first syllable isn't clipped).
func NewListener(chunks <-chan []byte, v VoiceDetector, preBufMS float64) *Listener {
	return &Listener{
		chunks:      chunks,
		vad:         v,
		utterances:  make(chan []byte),
		flushSignal: make(chan struct{}, 1),
		preBufMS:    preBufMS,
	}
}

// TurnJudge decides whether a paused utterance is a finished turn.
// Predict gets the whole utterance captured so far (48 kHz mono s16le, the
// same PCM contract as everything else in this package) and returns the
// probability the speaker is done. Defined here rather than importing a
// concrete model (internal/smartturn) so this package keeps depending only
// on what it drives, the same way VoiceDetector does; tests fake it.
type TurnJudge interface {
	Predict(pcm []byte) (float32, error)
}

// TurnJudgeConfig configures turn-judge endpointing (see SetTurnJudge).
// Threshold is the probability at or above which a paused utterance is
// delivered. RecheckMS is how much further continuous quiet triggers
// another judgment after a "keep waiting" one (<= 0: only re-ask at each
// new pause). MaxSilenceMS delivers unconditionally, without asking, once
// that much quiet has accumulated (<= 0: no cap) -- the backstop that
// keeps a judge that never says "done" from holding a turn forever.
type TurnJudgeConfig struct {
	Judge                   TurnJudge
	Threshold               float64
	RecheckMS, MaxSilenceMS float64
}

// SetTurnJudge turns on turn-judge endpointing: the detector's own ended
// (which the caller is expected to have configured to fire after a short
// pause, and to have reported via SetSilenceMS) no longer delivers the
// utterance by itself, it holds it open and asks cfg.Judge whether the
// speaker is done -- see Run for the full hold behavior. A nil cfg.Judge
// leaves today's fixed-silence endpointing untouched. Call before Run.
func (l *Listener) SetTurnJudge(cfg TurnJudgeConfig) {
	l.mu.Lock()
	l.judge = cfg
	l.mu.Unlock()
}

// SetMinSpeechMS drops a naturally-ended utterance -- by the detector's
// silence, the turn judge, or its quiet cap -- holding less than ms of
// speech, as measured by the detector's SpeechMeter over the delivered
// audio (pre-buffer included), instead of delivering it; OnSpeechAbort
// fires as for any discarded utterance. A turn needs at least the
// detector's onset debounce of speech to start at all, so this only
// matters above that: it filters a click, a cough, or the tail of
// talktome's own voice caught by the mic, which a short endpointing pause
// (a turn judge's) would otherwise turn into a whole bogus turn. A
// mute-finalized utterance is always delivered: the user chose to end it.
// Returns false, leaving the minimum off, when ms <= 0 or the detector
// can't measure speech (no SpeechMeter). Call before Run.
func (l *Listener) SetMinSpeechMS(ms float64) bool {
	if _, ok := l.vad.(SpeechMeter); !ok || ms <= 0 {
		return false
	}
	l.mu.Lock()
	l.minSpeechMS = ms
	l.mu.Unlock()
	return true
}

// chunkVoice is one pre-buffered chunk's size and speech (see preVoiced).
type chunkVoice struct {
	bytes int
	ms    float64
}

// lastVoicedLocked is the speech in the chunk just fed, 0 when the minimum
// is off. Caller holds mu.
func (l *Listener) lastVoicedLocked() float64 {
	if l.minSpeechMS <= 0 {
		return 0
	}
	return l.vad.(SpeechMeter).VoicedMS()
}

// tooShortLocked reports whether the utterance being delivered holds less
// speech than the minimum, logging the drop. Caller holds mu.
func (l *Listener) tooShortLocked() bool {
	if l.minSpeechMS <= 0 || l.voicedMS >= l.minSpeechMS {
		return false
	}
	log.Listen("utterance dropped: %dms of speech, below the %dms minimum", int(l.voicedMS), int(l.minSpeechMS))
	return true
}

// SetSilenceMS tells the listener how much trailing silence the detector
// waits for before ending an utterance, so LastEndpointWait can report it.
// Called once by app before Run; defaults to 0 (reported as unknown).
func (l *Listener) SetSilenceMS(ms float64) {
	l.mu.Lock()
	l.silenceMS = ms
	l.mu.Unlock()
}

// LastEndpointWait returns the endpointing wait of the most recently
// delivered utterance: the configured silence for a naturally ended one
// (with a turn judge set, the quiet time accumulated by the moment it was
// delivered -- see Run), 0 for a mute-finalized flush (nothing was waited
// for). It is set under l.mu
// before the utterance is sent and read by the turn loop right after the
// receive; the next delivery needs at least another onset+pause of audio, so
// it can't be overwritten before that read.
func (l *Listener) LastEndpointWait() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastWait
}

// Utterances returns the channel on which whole captured utterances are
// delivered. Closed when the input chunk channel closes.
func (l *Listener) Utterances() <-chan []byte {
	return l.utterances
}

// SetMuted mutes or unmutes the listener. While muted, incoming audio is
// discarded before ever reaching the detector (see Run) -- used to stop
// the app from "hearing" its own synthesized replies coming back through
// the microphone (there's no acoustic echo cancellation in this path).
//
// Muting while the VoiceDetector still thinks the user is speaking (e.g. a
// manual mute during an in-progress turn) always resets the detector
// (VoiceDetector.Reset) so it no longer believes it's mid-utterance --
// without that, unmuting later would leave a detector still expecting to
// be mid-speech fed fresh post-unmute audio, and it would splice the
// pre-mute fragment onto that with the muted gap silently excised.
//
// What happens to the partial capture itself depends on
// finalizeIfSpeaking. When true, it's finalized as a real utterance --
// delivered on Utterances() just like a naturally-ended one -- rather
// than silently discarded: a user who deliberately hits mute mid-sentence
// almost always wants whatever they'd already said processed, not thrown
// away. When false, it's discarded exactly as before this finalize
// behavior existed. Callers must pass true only for a mute the user
// themselves actually chose (see internal/app's setUserMuted/toggleMute);
// an automatic mute the harness itself triggers (e.g. muting for the
// duration of its own reply, see setHarnessMuted) must pass false, since
// there's no "the user is done talking" intent behind it -- a stray VAD
// onset that happens to be accumulating in the brief window before that
// automatic mute lands (ambient noise, a scheduler stall) has no business
// being promoted into a real turn just because Speaking() happened to be
// true at that instant. Muting before any utterance has started --
// nothing but the rolling pre-buffer accumulated -- has nothing to
// finalize regardless of finalizeIfSpeaking, so that's always discarded.
//
// An utterance held open waiting for the turn judge (see SetTurnJudge and
// Run) counts as speaking here even though the detector itself has
// already reported ended: it's still an utterance in progress the user
// hasn't been answered for, so the same finalize-or-discard rule applies,
// and the hold ends either way.
func (l *Listener) SetMuted(muted bool, finalizeIfSpeaking bool) {
	l.mu.Lock()
	l.muted = muted
	aborted := false
	if muted && (l.vad.Speaking() || l.holding) {
		if finalizeIfSpeaking && len(l.buf) > 0 && !l.closing {
			// !l.closing: Run has already committed to closing utterances
			// (see its shutdown's doc comment) -- enqueuing here would add
			// a flush nothing will ever drain. Discarding it instead is the
			// same graceful degrade Muting-before-any-utterance-started
			// already has, not a crash or a hang; it's also vanishingly
			// rare, since it requires this exact call to land after the
			// whole listener has already begun shutting down.
			l.pendingFlush = append(l.pendingFlush, l.buf)
		} else {
			aborted = true
		}
		l.vad.Reset()
		if l.holding {
			l.holding = false
			l.holdGen++
		}
	}
	if muted {
		l.preBuf = nil
		l.preVoiced = nil
		l.buf = nil
	}
	needSignal := len(l.pendingFlush) > 0
	l.mu.Unlock()

	if aborted && l.OnSpeechAbort != nil {
		l.OnSpeechAbort()
	}

	if needSignal {
		select {
		case l.flushSignal <- struct{}{}:
		default: // a wake-up is already pending; Run will drain the whole queue anyway
		}
	}

	if l.OnMuteChange != nil {
		l.OnMuteChange(muted)
	}
}

// IsMuted reports whether the listener is currently muted (see SetMuted).
func (l *Listener) IsMuted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.muted
}

// Speaking reports whether the voice-activity detector currently thinks the
// user is mid-utterance. Unlike IsMuted (which covers the whole turn, from
// end of utterance capture through the spoken reply), this catches the
// narrower window while the user is actively talking but the listener
// hasn't muted yet. Reports the detector only: a paused utterance held
// open for the turn judge (see Run) reads as not speaking here, since the
// user really is quiet during the hold.
func (l *Listener) Speaking() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.vad.Speaking()
}

// Busy reports whether the user has an utterance in progress or on its way
// to a receiver: the detector speaking, a paused utterance held for the
// turn judge, captured audio not yet delivered, a naturally-ended
// utterance whose send hasn't been received, or a queued mute-finalized
// flush. Unlike Speaking, it is what something that must never talk over
// the user, or mute the mic under them (a harness mute discards a hold),
// should check.
func (l *Listener) Busy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.vad.Speaking() || l.holding || len(l.buf) > 0 || l.pending != nil || len(l.pendingFlush) > 0
}

// LastSpeech returns when the listener last handled a chunk belonging to
// an utterance -- roughly when the user last spoke, whether that utterance
// was then delivered or discarded. Zero if never.
func (l *Listener) LastSpeech() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSpeech
}

// pendingDelivery is one naturally-ended utterance on its way to a
// receiver (see DropPendingDelivery). cancel is closed by
// DropPendingDelivery to pull Run out of its blocked send; cancelled and
// sending are guarded by Listener.mu; sent is written by Run before it
// closes done, and only read by DropPendingDelivery after done is closed.
type pendingDelivery struct {
	cancel    chan struct{}
	done      chan struct{}
	cancelled bool
	sending   bool
	sent      bool
}

// beginDeliveryLocked registers a new pending delivery. Caller holds mu,
// in the same hold that takes the utterance out of buf.
func (l *Listener) beginDeliveryLocked() *pendingDelivery {
	p := &pendingDelivery{cancel: make(chan struct{}), done: make(chan struct{})}
	l.pending = p
	return p
}

// deliver hands a naturally-ended utterance to a receiver unless
// DropPendingDelivery cancels it first. Only called from Run's goroutine.
// A drop that lands before the send starts always wins (sending is never
// set); one that lands while the send is blocked races a receiver in the
// select, and whichever happened is recorded in p.sent so
// DropPendingDelivery reports it truthfully.
func (l *Listener) deliver(p *pendingDelivery, utterance []byte) {
	l.mu.Lock()
	if p.cancelled {
		l.mu.Unlock()
		close(p.done)
		return
	}
	p.sending = true
	l.mu.Unlock()

	sent := false
	select {
	case l.utterances <- utterance:
		sent = true
	case <-p.cancel:
	}

	l.mu.Lock()
	p.sent = sent
	if l.pending == p {
		l.pending = nil
	}
	l.mu.Unlock()
	close(p.done)
}

// DropPendingDelivery discards the utterance Run has finished capturing
// but not yet handed to a receiver, returning true if there was one. Used
// by barge-in: while the assistant is thinking the mic stays live, and
// when that window closes an utterance that ended inside it but hasn't
// been received must not surface later as a phantom turn. Returns false
// when nothing is pending -- including when the receiver won the race and
// took the utterance, which is why it may briefly wait for Run's blocked
// send to settle (bounded: the cancel case is always ready). It only ever
// waits once Run is inside that send, so a hook running on Run's own
// goroutine (OnSpeechStart, OnSustainedSpeech) can call it without
// deadlocking -- it sees sending unset and returns at once.
//
// Mute-finalized flushes (see SetMuted) are not droppable: they keep
// their own blocking path through drain, since a user who muted
// mid-sentence asked for what they said to be processed, and that path's
// ordering and shutdown guarantees (see closing) stay untouched.
func (l *Listener) DropPendingDelivery() bool {
	l.mu.Lock()
	p := l.pending
	if p == nil {
		l.mu.Unlock()
		return false
	}
	l.pending = nil
	p.cancelled = true
	close(p.cancel)
	sending := p.sending
	l.mu.Unlock()

	if !sending {
		// deliver will see cancelled before it ever sends.
		log.Listen("utterance dropped before delivery")
		return true
	}
	<-p.done
	if p.sent {
		return false
	}
	log.Listen("utterance dropped before delivery")
	return true
}

// drain delivers every currently queued mute-finalized utterance, in
// order, blocking on each send in turn, until the queue is observed
// empty -- at which point onEmpty runs inside that same lock hold (before
// drain unlocks and returns), so a caller that needs to atomically flip
// some state exactly when the queue empties (see drainPendingFlushesAndClose)
// can do so with no gap a concurrent SetMuted could land in. Only ever
// called from Run's own goroutine, which is also the only goroutine that
// closes utterances -- so a flush delivered here can never race that
// close (see Run).
func (l *Listener) drain(onEmpty func()) {
	for {
		l.mu.Lock()
		if len(l.pendingFlush) == 0 {
			onEmpty()
			l.mu.Unlock()
			return
		}
		flush := l.pendingFlush[0]
		l.pendingFlush = l.pendingFlush[1:]
		l.lastWait = 0
		l.mu.Unlock()
		l.utterances <- flush
	}
}

// drainPendingFlushes is drain with no special end-of-queue action -- the
// ordinary per-flushSignal case during normal operation, where anything
// appended after this returns will always get its own later flushSignal
// to trigger another drain, unlike drainPendingFlushesAndClose's
// shutdown-time call, which has no such later chance.
func (l *Listener) drainPendingFlushes() {
	l.drain(func() {})
}

// drainPendingFlushesAndClose is drain's shutdown-time use, and the only
// place closing is ever set. Unlike a mid-run drain, this is the *last*
// chance to ever deliver a queued flush -- there's no future flushSignal
// to catch one appended after this returns -- so "the queue is empty" and
// "no new flush can ever be appended again" must be a single atomic
// observation, not two separate ones with a gap a concurrent SetMuted
// could land in (see closing's own doc comment for the data-loss bug that
// gap caused). Passing the closing=true assignment as drain's onEmpty
// callback is what makes it run inside the very same lock hold that
// observes the queue empty, rather than as a separate step after.
func (l *Listener) drainPendingFlushesAndClose() {
	l.drain(func() { l.closing = true })
	close(l.utterances)
}

// Run drives the listener loop: read chunks, feed the vad, buffer, and
// deliver whole utterances -- both naturally-ended ones and mute-finalized
// ones queued by SetMuted -- on Utterances(). Run's own goroutine is the
// only sender on utterances and the only one that ever closes it, by
// construction (SetMuted only ever queues a flush into pendingFlush and
// nudges flushSignal; it never touches utterances directly), which is what
// makes a send-after-close panic or an out-of-order delivery between two
// flushes structurally impossible rather than something requiring its own
// extra coordination to prevent. Blocks until the input channel closes, at
// which point any still-queued flush is drained first (see
// drainPendingFlushesAndClose; best-effort if nothing is left to ever
// drain Utterances() -- that blocks forever on the last send, only
// reachable during the whole process's own final shutdown and reclaimed
// harmlessly along with everything else then) and only then is
// Utterances() itself closed. Run this in its own goroutine.
//
// A naturally-ended utterance's send is the one cancellable delivery:
// DropPendingDelivery can pull Run out of it (see deliver), which leaves
// every guarantee above intact -- Run is still the only sender, it still
// finishes (or abandons) that send before touching flushes or the close,
// and a flush's own blocking send is never cancellable.
//
// With a turn judge set (SetTurnJudge), the detector's ended is only a
// pause: the utterance is held open and the judge asked right away. While
// held, every chunk keeps being appended to the same utterance -- quiet
// and resumed speech alike, since a resumed onset is the speaker carrying
// on, not a new turn (so no pre-buffer splice, no OnSpeechStart, no
// "speech detected" line). Quiet time accumulates on chunks the detector
// calls non-speech, starting from the pause already waited out; the judge
// is asked again every RecheckMS of further quiet, and again immediately
// at the next pause if speech resumed. The held utterance is delivered
// when the judge says done, when quiet reaches MaxSilenceMS (without
// asking), or when the judge errors (fail open: answering a little early
// beats never answering). A hold still pending when the input channel
// closes is dropped, exactly like an in-progress capture on the plain
// path.
//
// The judge runs inline on this goroutine, on a copy of the utterance
// taken under mu, with the call itself made outside mu so SetMuted and
// the other accessors never wait on a model. Inline (rather than on a
// helper goroutine) keeps "Run is the only goroutine touching buf's
// delivery" true without new coordination; the mic channel's buffer
// (audioio.MicChunkBufferSize chunks) absorbs a judgment's latency.
func (l *Listener) Run() {
	defer l.drainPendingFlushesAndClose()
	preBufMaxBytes := int(float64(SampleRate) * 2 * l.preBufMS / 1000)

	for {
		select {
		case chunk, ok := <-l.chunks:
			if !ok {
				return
			}
			l.handleChunk(chunk, preBufMaxBytes)
		case <-l.flushSignal:
			l.drainPendingFlushes()
		}
	}
}

// handleChunk is one iteration of Run's chunk branch, split out so the
// judge's lock-release/re-acquire dance stays readable. Only ever called
// from Run's goroutine.
func (l *Listener) handleChunk(chunk []byte, preBufMaxBytes int) {
	l.mu.Lock()
	if l.muted {
		l.mu.Unlock()
		return
	}

	if l.OnLevel != nil {
		l.OnLevel(audiolevel.RMS(chunk))
	}

	wasSpeaking := l.vad.Speaking()
	wasHolding := l.holding
	prevWait := l.lastWait
	started, ended := l.vad.Feed(chunk)
	voiced := l.lastVoicedLocked()

	if !wasHolding && !wasSpeaking && !started {
		l.preBuf = append(l.preBuf, chunk...)
		if l.minSpeechMS > 0 {
			l.preVoiced = append(l.preVoiced, chunkVoice{len(chunk), voiced})
		}
		if over := len(l.preBuf) - preBufMaxBytes; over > 0 {
			l.preBuf = l.preBuf[over:]
			// Forget chunks wholly scrolled out; one partly kept still counts.
			kept := 0
			for _, c := range l.preVoiced {
				kept += c.bytes
			}
			for len(l.preVoiced) > 0 && kept-l.preVoiced[0].bytes >= len(l.preBuf) {
				kept -= l.preVoiced[0].bytes
				l.preVoiced = l.preVoiced[1:]
			}
		}
		l.mu.Unlock()
		return
	}

	l.lastSpeech = time.Now()
	newUtterance := started && !wasHolding
	if newUtterance {
		l.buf = append(append([]byte{}, l.preBuf...), chunk...)
		l.preBuf = nil
		l.voicedMS = voiced
		for _, c := range l.preVoiced {
			l.voicedMS += c.ms
		}
		l.preVoiced = nil
		l.speechMS, l.sustainedFired = 0, false
		log.Listen("speech detected (%s) -- listening...", l.vad.DetectionLabel())
	} else {
		l.buf = append(l.buf, chunk...)
		l.voicedMS += voiced
		if started { // speech resumed during a hold: same utterance
			l.quietMS = 0
		}
	}

	chunkMS := float64(len(chunk)/2) * 1000 / SampleRate
	if started || l.vad.Speaking() {
		l.speechMS += chunkMS
	}
	sustained := false
	if l.SustainedSpeechMS > 0 && l.OnSustainedSpeech != nil && !l.sustainedFired && l.speechMS >= l.SustainedSpeechMS {
		l.sustainedFired = true
		sustained = true
	}

	judged := l.judge.Judge != nil
	deliver, ask := false, false
	switch {
	case ended && judged:
		// The detector's pause: (re)enter the hold with the pause it
		// already waited out counted as quiet, and ask right away.
		l.holding = true
		l.quietMS = l.silenceMS
		ask = true
	case ended:
		deliver = true
		l.lastWait = time.Duration(l.silenceMS * float64(time.Millisecond))
	case l.holding && !l.vad.Speaking():
		l.quietMS += chunkMS
		ask = l.judge.RecheckMS > 0 && l.quietMS >= l.nextCheckMS
	}
	if l.holding && l.judge.MaxSilenceMS > 0 && l.quietMS >= l.judge.MaxSilenceMS {
		log.Listen("end of turn: %s of quiet -> answering", l.quietDuration().Round(100*time.Millisecond))
		deliver, ask = true, false
		l.holding = false
		l.lastWait = l.quietDuration()
	}

	var utterance, snapshot []byte
	var p *pendingDelivery
	dropped := false
	if deliver {
		// Snapshot and clear buf under the same lock SetMuted uses to
		// write it, so a concurrent mute can't race this read/reset --
		// and register the delivery in that same hold, so
		// DropPendingDelivery can never miss an utterance in between.
		utterance = l.buf
		l.buf = nil
		if dropped = l.tooShortLocked(); dropped {
			l.lastWait = prevWait // describes the last *delivered* utterance
		} else {
			p = l.beginDeliveryLocked()
		}
	}
	cfg, gen := l.judge, l.holdGen
	if ask {
		snapshot = append([]byte(nil), l.buf...)
	}
	l.mu.Unlock()

	if newUtterance && l.OnSpeechStart != nil {
		l.OnSpeechStart()
	}
	if sustained {
		l.OnSustainedSpeech()
	}

	if ask {
		utterance, p, dropped = l.judgeHeld(cfg, gen, snapshot)
	}
	if dropped && l.OnSpeechAbort != nil {
		l.OnSpeechAbort()
	}

	if p != nil {
		dur := audioutil.PCMDuration(utterance, SampleRate)
		l.mu.Lock()
		voiced, metered := l.voicedMS, l.minSpeechMS > 0
		l.mu.Unlock()
		if metered {
			// The speech figure is what -vad-min-speech-ms compares against,
			// shown so it can be tuned from real utterances.
			log.Listen("utterance captured (%s audio, %dms speech)", dur.Round(100*time.Millisecond), int(voiced))
		} else {
			log.Listen("utterance captured (%s audio)", dur.Round(100*time.Millisecond))
		}
		l.deliver(p, utterance)
	}
}

// judgeHeld asks the turn judge about the held utterance snapshot (taken
// under mu at hold generation gen), outside mu, then re-locks to act on
// the answer: returns the utterance to deliver and its registered pending
// delivery when the turn is done, or schedules the next quiet-time recheck
// and returns a nil delivery. dropped reports a done turn discarded for
// holding too little speech (see SetMinSpeechMS).
// A hold a concurrent SetMuted has ended in the meantime (holdGen moved
// on) is left alone -- that mute already finalized or discarded it.
func (l *Listener) judgeHeld(cfg TurnJudgeConfig, gen uint64, snapshot []byte) (utterance []byte, p *pendingDelivery, dropped bool) {
	prob, err := cfg.Judge.Predict(snapshot)
	done := err != nil || float64(prob) >= cfg.Threshold
	if err != nil {
		l.judgeWarn.Do(func() {
			log.Warnf("turn judge failed, answering without it: %v", err)
		})
		log.Listen("end of turn? judge error -> answering")
	} else {
		decision := "waiting"
		if done {
			decision = "answering"
		}
		log.Listen("end of turn? p=%.2f -> %s", prob, decision)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.holding || l.holdGen != gen {
		return nil, nil, false
	}
	if !done {
		l.nextCheckMS = l.quietMS + cfg.RecheckMS
		return nil, nil, false
	}
	utterance = l.buf
	l.buf = nil
	l.holding = false
	if l.tooShortLocked() {
		return nil, nil, true
	}
	l.lastWait = l.quietDuration()
	return utterance, l.beginDeliveryLocked(), false
}

// quietDuration is quietMS as a time.Duration. Caller holds mu.
func (l *Listener) quietDuration() time.Duration {
	return time.Duration(l.quietMS * float64(time.Millisecond))
}
