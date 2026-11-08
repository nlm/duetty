// Package app is duetty's composition root: it wires the microphone, Silero
// VAD, Smart Turn, trimming, transcription, translation and the two-pane UI.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nlm/duetty/internal/audioio"
	"github.com/nlm/duetty/internal/config"
	"github.com/nlm/duetty/internal/convo"
	"github.com/nlm/duetty/internal/denoise"
	"github.com/nlm/duetty/internal/langid"
	"github.com/nlm/duetty/internal/live"
	"github.com/nlm/duetty/internal/log"
	"github.com/nlm/duetty/internal/silerovad"
	"github.com/nlm/duetty/internal/smartturn"
	"github.com/nlm/duetty/internal/stt"
	"github.com/nlm/duetty/internal/translate"
	"github.com/nlm/duetty/internal/trim"
	"github.com/nlm/duetty/internal/ui"
	"github.com/nlm/duetty/internal/usage"
	"github.com/nlm/duetty/internal/vad"
)

// MicOff is the -mic value that disables audio capture entirely (typed input
// only; no VAD, Silero or Smart Turn is loaded).
const MicOff = "off"

const (
	levelInterval = 100 * time.Millisecond // status meter throttle
	meterMaxRMS   = 8000.0                 // RMS shown as a full meter (loud speech)
	utteranceQ    = 32                     // utterances queued for trimming
	listenerJoin  = time.Second            // shutdown wait for the VAD goroutine
	liveLockAfter = 3 * time.Second        // live recording length that locks its language
)

// noticeShown is how long startup notices stay in the status bar (a var for tests).
var noticeShown = 10 * time.Second

// UI is what Run drives; *ui.App satisfies it. Tests pass a terminal-less fake.
type UI interface {
	SetRows([]convo.Row)
	SetStatus(level float64, state string, inFlight int, muted bool)
	SetNotice(string)
	SetUsage(string)
	OnSubmit(func(string))
	OnCancel(func())
	OnMuteToggle(func())
	Run() error
	Stop()
}

// Audio replaces the microphone + Silero pipeline (tests). Chunks is 48 kHz
// s16le mono PCM, Detector drives the Listener, Probs gives per-window speech
// probabilities for trimming. Notices are short startup notes shown in the
// status bar for a while (e.g. "smart turn off").
type Audio struct {
	Chunks   <-chan []byte
	Detector vad.VoiceDetector
	Probs    func(pcm48k []byte) ([]float32, error)
	Notices  []string
}

// Deps lets tests replace the network clients, the UI, the audio pipeline and
// the log sink. Zero values mean "build the real one from the config".
type Deps struct {
	Resolver   convo.Resolver
	Translator translate.Translator
	UI         UI
	Audio      *Audio
	// LogSink receives every log line. nil: -log-file, else discarded.
	LogSink log.Sink
	// Stderr receives startup warnings before the UI starts (nil: os.Stderr).
	Stderr io.Writer
	// Meter counts session usage (nil: a new one). Run sets its onChange.
	Meter *usage.Meter
}

// Run starts duetty and blocks until the UI quits. Startup errors (no API key,
// Silero or onnxruntime missing, mic open failure) are returned before the UI
// starts; the caller prints them.
func Run(ctx context.Context, cfg config.Config, d Deps) (err error) {
	if cfg.APIKey == "" && cfg.NeedsKey() {
		return errors.New("no API key: set DUETTY_API_KEY (or OPENAI_API_KEY), or api_key in the config file")
	}
	stderr := d.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Logging. Until the UI starts, warnings also go to stderr; once it runs
	// nothing may touch the terminal, so only the file (or discard) remains.
	var runSink log.Sink = discardSink{}
	switch {
	case d.LogSink != nil:
		runSink = d.LogSink
	case cfg.LogFile != "":
		f, ferr := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if ferr != nil {
			return fmt.Errorf("log file: %w", ferr)
		}
		fs := &fileSink{w: f}
		runSink = fs
		defer func() {
			log.SetSink(discardSink{})
			fs.close()
		}()
	}
	log.SetSink(teeSink{runSink, &warnSink{w: stderr}})
	defer log.SetSink(discardSink{}) // nothing reaches the console after the UI

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pair := cfg.Languages
	log.Info("duetty starting: %s/%s, stt=%s, translate=%s", pair[0], pair[1], cfg.STTModel, cfg.TranslateModel)

	// Usage counter: always on, shown at the end of the status bar.
	up := &usagePusher{prices: cfg.Prices()}
	meter := d.Meter
	if meter == nil {
		meter = usage.NewMeter(nil)
	}
	meter.SetOnChange(up.push)
	up.meter = meter

	resolver := d.Resolver
	if resolver == nil {
		sc := stt.NewClient(cfg.STTURL(), cfg.APIKey, cfg.STTModel)
		sc.SetMeter(meter)
		resolver = &stt.Resolver{
			T:              sc,
			ID:             langid.New(pair),
			Pair:           pair,
			LogprobFloor:   cfg.NoiseMinLogprob,
			MaxCompression: cfg.NoiseMaxCompression,
		}
	}
	translator := d.Translator
	if translator == nil {
		tc := translate.NewClient(cfg.TranslateURL(), cfg.APIKey, cfg.TranslateModel)
		tc.SetMeter(meter)
		translator = tc
	}

	// Audio pipeline (unless -mic off).
	var (
		audio   *Audio
		closers []func()
	)
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}()
	micOff := cfg.Mic == MicOff && d.Audio == nil
	var judge vad.TurnJudge
	detSilence := cfg.VADEndSilenceMS
	switch {
	case d.Audio != nil:
		audio = d.Audio
	case micOff:
		log.Info("mic off: typed input only")
	default:
		audio, judge, detSilence, closers, err = openAudio(ctx, cfg)
		if err != nil {
			return err
		}
	}

	st := &status{micOff: micOff}

	u := d.UI
	if u == nil {
		u = ui.New(pair, cfg.Timestamps)
	}
	rp := &rowPusher{ui: u, st: st, outcomes: &outcomeLog{seen: map[uint64]convo.Status{}}}
	cl := convo.New(pair, resolver, translator, cfg.TranslateContext, rp.changed)
	rp.log = cl
	st.ui, st.log = u, cl
	up.mu.Lock()
	up.ui = u
	up.mu.Unlock()

	// Live preview: only with the mic path; otherwise prev stays nil and
	// every utterance goes through SubmitAudio as before.
	var prev *live.Previewer
	if cfg.LivePreview && audio != nil {
		prev = live.New(live.Config{
			IntervalMS: cfg.LivePreviewIntervalMS,
			MinMS:      cfg.VADMinSpeechMS,
			LockAfter:  liveLockAfter,
			Timeout:    convo.RequestTimeout,
		}, refreshLogger{resolver}, cl)
		log.Info("live preview on: refresh every %.0f ms", cfg.LivePreviewIntervalMS)
	}

	var lst *vad.Listener
	u.OnSubmit(func(text string) {
		log.Info("typed input submitted (%d bytes)", len(text))
		cl.SubmitText(text)
	})
	u.OnCancel(func() {
		log.Info("cancel requested (%d in flight)", cl.InFlight())
		cl.CancelPending()
		if prev != nil && lst != nil {
			lst.DropCurrent() // its OnSpeechAbort drops the live turn
		}
	})
	u.OnMuteToggle(func() {
		if lst != nil {
			lst.SetMuted(!lst.IsMuted(), true)
		}
	})

	var listenerDone chan struct{}
	var workerDone chan struct{}
	if audio != nil {
		lst = vad.NewListener(audio.Chunks, audio.Detector, cfg.VADPreSpeechMS)
		lst.SetSilenceMS(detSilence)
		if cfg.VADMinSpeechMS > 0 && !lst.SetMinSpeechMS(cfg.VADMinSpeechMS) {
			log.Warnf("vad_min_speech_ms ignored: detector cannot measure speech")
		}
		if judge != nil {
			lst.SetTurnJudge(vad.TurnJudgeConfig{
				Judge:        judge,
				Threshold:    cfg.SmartTurnThreshold,
				RecheckMS:    cfg.SmartTurnRecheckMS,
				MaxSilenceMS: cfg.SmartTurnMaxSilenceMS,
			})
		}
		var lastLevel time.Time // only touched on the Listener goroutine
		lst.OnLevel = func(rms float64) {
			now := time.Now()
			if now.Sub(lastLevel) < levelInterval {
				return
			}
			lastLevel = now
			st.setLevel(math.Min(rms/meterMaxRMS, 1))
		}
		lst.OnSpeechStart = func() {
			log.Info("speech detected")
			st.speechStarted()
		}
		lst.OnSpeechAbort = func() {
			log.Info("utterance discarded")
			if prev != nil {
				prev.Abort()
			}
			st.speechEnded()
		}
		if prev != nil {
			lst.OnAudio = prev.Audio
		}
		lst.OnMuteChange = func(m bool) {
			log.Info("muted=%v", m)
			st.setMuted(m)
		}

		queue := make(chan utterance, utteranceQ)
		var probsMu sync.Mutex
		probs := func(pcm []byte) ([]float32, error) {
			probsMu.Lock()
			defer probsMu.Unlock()
			return audio.Probs(pcm)
		}
		margin := time.Duration(cfg.VADTrimMarginMS * float64(time.Millisecond))

		listenerDone = make(chan struct{})
		go func() {
			defer close(listenerDone)
			lst.Run()
		}()
		// Delivery: drain the Listener promptly, hand off to the worker.
		go func() {
			defer close(queue)
			for pcm := range lst.Utterances() {
				st.speechEnded()
				ut := utterance{pcm: pcm}
				if prev != nil {
					// Paired here, in delivery order, so a dropped
					// utterance cannot shift the pairing of later ones.
					ut.pcm, ut.liveID, ut.lang = prev.End(pcm)
				}
				select {
				case queue <- ut:
				default:
					log.Warnf("utterance queue full, dropping one")
					if ut.liveID != 0 {
						cl.DropLive(ut.liveID)
					}
				}
			}
			if ctx.Err() == nil {
				log.Warnf("microphone stream ended")
				st.setMicLost()
			}
		}()
		// Worker: trim → WAV → submit, one at a time so capture order holds.
		workerDone = make(chan struct{})
		go func() {
			defer close(workerDone)
			for ut := range queue {
				if ctx.Err() != nil {
					continue
				}
				wav := trimToWAV(ut.pcm, probs, cfg.VADThreshold, margin)
				switch {
				case ut.liveID != 0 && wav != nil:
					cl.FinishLive(ut.liveID, wav, ut.lang)
				case ut.liveID != 0: // nothing left after the early commits
					cl.DropLive(ut.liveID)
				case wav != nil:
					cl.SubmitAudio(wav)
				}
			}
		}()
	}

	go func() { // external cancellation (signal) also quits the UI
		<-ctx.Done()
		u.Stop()
	}()
	// Startup warnings went to stderr, which the UI is about to cover: show
	// them briefly in the status bar too.
	if audio != nil && len(audio.Notices) > 0 {
		u.SetNotice(strings.Join(audio.Notices, " · "))
		t := time.AfterFunc(noticeShown, func() { u.SetNotice("") })
		defer t.Stop()
	}
	st.push()
	up.push()
	log.SetSink(runSink) // UI owns the terminal from here on
	runErr := u.Run()
	log.Info("shutting down")

	cancel()
	cl.CancelPending()
	if listenerDone != nil {
		select {
		case <-listenerDone:
		case <-time.After(listenerJoin):
			// Its detector may still be in use: leave the ONNX sessions
			// to process exit rather than closing them under it.
			log.Warnf("listener did not stop in time")
			closers = nil
		}
		select {
		case <-workerDone:
		case <-time.After(listenerJoin):
			log.Warnf("utterance worker did not stop in time")
		}
	}
	if prev != nil {
		prev.Close()
	}
	return runErr
}

// utterance is a delivered utterance waiting for the worker. With live
// preview, pcm is the part not yet committed early and liveID (0: none)
// the live entry its final pass replaces.
type utterance struct {
	pcm    []byte
	liveID convo.LiveID
	lang   string // locked language of the live turn, "" for a full resolve
}

// refreshLogger logs each live preview refresh: its outcome (ok, noise or a
// short error reason), durations and counts -- never text.
type refreshLogger struct{ r convo.Resolver }

func (l refreshLogger) ResolveLocked(ctx context.Context, wav16k []byte, lang string) (stt.Resolved, error) {
	start := time.Now()
	res, err := l.r.ResolveLocked(ctx, wav16k, lang)
	outcome := "ok"
	switch {
	case errors.Is(err, stt.ErrNoise):
		outcome = "noise"
	case err != nil:
		outcome = convo.ShortError(err)
	}
	log.Info("live refresh: %s, %s audio, took %s", outcome, wavDuration(wav16k), time.Since(start).Round(time.Millisecond))
	if n := live.CommitCount(res.Segments); err == nil && n > 0 {
		log.Info("live commit: %d segments", n)
	}
	return res, err
}

// wavDuration is the length of a 16 kHz mono s16le WAV (44-byte header).
func wavDuration(wav []byte) time.Duration {
	return (time.Duration(max(len(wav)-44, 0)/2) * time.Second / 16000).Round(10 * time.Millisecond)
}

// trimToWAV trims a captured utterance to its voiced part and encodes it for
// transcription; nil means nothing is left to send. Logs durations only,
// never content.
func trimToWAV(pcm []byte, probs func([]byte) ([]float32, error), threshold float64, margin time.Duration) []byte {
	start := time.Now()
	captured := pcmDuration(pcm)
	p, err := probs(pcm)
	out := pcm
	if err != nil {
		log.Warnf("trim: speech probabilities failed, sending untrimmed: %v", err)
	} else {
		out = trim.Trim(pcm, p, threshold, margin)
		if out == nil {
			log.Info("utterance dropped: no voice after trim (%s captured)", captured)
			return nil
		}
	}
	wav, err := stt.PCM48kToWAV16k(out)
	if err != nil {
		log.Warnf("utterance dropped: wav encode: %v", err)
		return nil
	}
	log.Info("utterance submitted: %s captured, %s after trim, trim took %s",
		captured, pcmDuration(out), time.Since(start).Round(time.Millisecond))
	return wav
}

func pcmDuration(pcm []byte) time.Duration {
	return (time.Duration(len(pcm)/2) * time.Second / audioio.SampleRate).Round(10 * time.Millisecond)
}

// openAudio loads Silero (two instances: one drives the Listener, one is
// dedicated to trimming), optionally Smart Turn, and opens the microphone.
// Any failure here is fatal except Smart Turn and denoise, which warn.
func openAudio(ctx context.Context, cfg config.Config) (a *Audio, judge vad.TurnJudge, silenceMS float64, closers []func(), err error) {
	fail := func(e error) (*Audio, vad.TurnJudge, float64, []func(), error) {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		return nil, nil, 0, nil, e
	}
	cfg, warns, perr := resolveAudioPaths(cfg, osPathEnv())
	for _, w := range warns {
		log.Warnf("%s", w)
	}
	if perr != nil {
		return fail(perr)
	}
	log.Info("audio paths: silero=%s smart_turn=%s denoise=%s onnxruntime=%s", cfg.VADModel, cfg.SmartTurnModel, cfg.DenoiseModel, cfg.ONNXRuntimeLib)
	if _, serr := os.Stat(cfg.VADModel); serr != nil {
		return fail(fmt.Errorf("silero VAD model: %w", serr))
	}

	silenceMS = cfg.VADEndSilenceMS
	var notices []string
	if cfg.SmartTurn {
		if cfg.SmartTurnModel == "" { // already warned by resolveAudioPaths
			notices = append(notices, "smart turn off")
		} else {
			det, serr := smartturn.New(cfg.ONNXRuntimeLib, cfg.SmartTurnModel, "", 0)
			if serr != nil {
				log.Warnf("smart turn unavailable, using fixed silence: %v", serr)
				notices = append(notices, "smart turn off")
			} else {
				judge, silenceMS = det, cfg.SmartTurnPauseMS
				closers = append(closers, det.Close)
				log.Info("end-of-turn: smart turn")
			}
		}
	}

	mainVAD, err := silerovad.NewVAD(cfg.ONNXRuntimeLib, cfg.VADModel, "", cfg.VADThreshold, silenceMS, cfg.VADOnsetMS, audioio.SampleRate)
	if err != nil {
		return fail(fmt.Errorf("silero VAD: %w", err))
	}
	closers = append(closers, mainVAD.Close)
	trimVAD, err := silerovad.NewVAD(cfg.ONNXRuntimeLib, cfg.VADModel, "", cfg.VADThreshold, silenceMS, cfg.VADOnsetMS, audioio.SampleRate)
	if err != nil {
		return fail(fmt.Errorf("silero VAD (trim): %w", err))
	}
	// A late trim after shutdown must never touch a released session.
	tv := &guardedVAD{v: trimVAD}
	closers = append(closers, tv.close)

	chunks, err := audioio.NewMic(cfg.Mic).Stream(ctx)
	if err != nil {
		return fail(fmt.Errorf("microphone: %w (use -mic off for typed input only)", err))
	}
	if cfg.Denoise && cfg.DenoiseModel == "" { // already warned by resolveAudioPaths
		notices = append(notices, "denoise off")
	}
	if cfg.Denoise && cfg.DenoiseModel != "" {
		d, derr := denoise.New(cfg.ONNXRuntimeLib, cfg.DenoiseModel)
		if derr != nil {
			log.Warnf("denoise unavailable: %v", derr)
			notices = append(notices, "denoise off")
		} else {
			// Close only stops inference; a chunk still in flight passes through.
			closers = append(closers, d.Close)
			chunks = denoise.Wrap(chunks, d)
			log.Info("audio: denoise on (%s, +%d ms latency)", cfg.DenoiseModel, denoise.LatencySamples*1000/audioio.SampleRate)
		}
	}
	log.Info("audio: silero VAD, silence %.0f ms", silenceMS)
	return &Audio{Chunks: chunks, Detector: mainVAD, Probs: tv.probs, Notices: notices}, judge, silenceMS, closers, nil
}

// guardedVAD makes trimming safe against Close at shutdown.
type guardedVAD struct {
	mu sync.Mutex
	v  *silerovad.VAD
}

func (g *guardedVAD) probs(pcm []byte) ([]float32, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.v == nil {
		return nil, errors.New("trim VAD closed")
	}
	return g.v.Probs(pcm)
}

func (g *guardedVAD) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.v != nil {
		g.v.Close()
		g.v = nil
	}
}

// rowPusher is the convo.Log onChange handler. convo calls it concurrently
// from several worker goroutines (and the UI event loop); snapshot and
// SetRows happen under one lock so an older snapshot can never be delivered
// after a newer one (SetRows never blocks, so holding the lock is safe).
type rowPusher struct {
	ui       UI
	log      *convo.Log
	st       *status
	outcomes *outcomeLog

	mu sync.Mutex
}

func (p *rowPusher) changed() {
	p.mu.Lock()
	rows := p.log.Rows()
	p.ui.SetRows(rows)
	p.mu.Unlock()
	p.st.push()
	p.outcomes.observe(rows)
}

// usagePusher shows the usage counter. The meter calls push concurrently
// from several request goroutines; snapshot and SetUsage happen under one
// lock so an older total is never shown after a newer one (as rowPusher).
type usagePusher struct {
	meter  *usage.Meter
	prices *usage.Prices

	mu sync.Mutex
	ui UI // nil until the UI exists; guarded by mu
}

func (p *usagePusher) push() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ui != nil {
		p.ui.SetUsage(usage.Format(p.meter.Totals(), p.prices))
	}
}

// status holds the status-bar state and pushes it to the UI.
type status struct {
	ui     UI
	log    *convo.Log
	micOff bool

	// pushMu serialises push (InFlight read + SetStatus) so a stale count
	// read by one goroutine cannot overwrite a newer one.
	pushMu sync.Mutex

	mu    sync.Mutex
	level float64
	// started counts OnSpeechStart; ended counts utterances delivered or
	// discarded. Every delivered or discarded utterance had exactly one
	// start, so "hearing" is started > ended. Counting (rather than a bool
	// cleared by the delivery goroutine) means a late delivery of utterance N
	// can never clear the "hearing" of utterance N+1 that already started.
	started, ended uint64
	muted          bool
	micLost        bool
}

func (s *status) setLevel(v float64) {
	s.mu.Lock()
	s.level = v
	s.mu.Unlock()
	s.push()
}

func (s *status) speechStarted() {
	s.mu.Lock()
	s.started++
	s.mu.Unlock()
	s.push()
}

func (s *status) speechEnded() {
	s.mu.Lock()
	if s.ended < s.started {
		s.ended++
	}
	s.mu.Unlock()
	s.push()
}

func (s *status) setMuted(m bool) {
	s.mu.Lock()
	s.muted = m
	s.level = 0 // OnLevel stops while muted
	s.mu.Unlock()
	s.push()
}

func (s *status) setMicLost() {
	s.mu.Lock()
	s.micLost, s.level, s.ended = true, 0, s.started
	s.mu.Unlock()
	s.push()
}

func (s *status) push() {
	if s.ui == nil || s.log == nil {
		return
	}
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	s.mu.Lock()
	state := "listening"
	switch {
	case s.micOff || s.micLost:
		state = "mic off"
	case s.muted:
		state = "muted"
	case s.started > s.ended:
		state = "hearing"
	}
	level, muted := s.level, s.muted
	s.mu.Unlock()
	s.ui.SetStatus(level, state, s.log.InFlight(), muted)
}

// outcomeLog logs each row's stage changes with latency and the short
// user-facing error -- never the row's text.
type outcomeLog struct {
	mu   sync.Mutex
	seen map[uint64]convo.Status
}

func (o *outcomeLog) observe(rows []convo.Row) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range rows {
		if prev, ok := o.seen[r.Seq]; ok && prev == r.Status {
			continue
		}
		o.seen[r.Seq] = r.Status
		age := time.Since(r.At).Round(time.Millisecond)
		if r.Status == convo.Failed {
			log.Warnf("row %d (%s): %s after %s: %s", r.Seq, r.Lang, r.Status, age, r.Err)
			continue
		}
		log.Info("row %d (%s): %s after %s", r.Seq, r.Lang, r.Status, age)
	}
}

// Sinks.

type discardSink struct{}

func (discardSink) Info(string, ...any)  {}
func (discardSink) Warnf(string, ...any) {}

type teeSink []log.Sink

func (t teeSink) Info(f string, a ...any) {
	for _, s := range t {
		s.Info(f, a...)
	}
}

func (t teeSink) Warnf(f string, a ...any) {
	for _, s := range t {
		s.Warnf(f, a...)
	}
}

// warnSink prints warnings only (startup, before the UI owns the terminal).
type warnSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *warnSink) Info(string, ...any) {}
func (s *warnSink) Warnf(f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "duetty: warning: %s\n", fmt.Sprintf(f, a...))
}

// fileSink writes timestamped lines to the -log-file.
type fileSink struct {
	mu     sync.Mutex
	w      io.WriteCloser
	closed bool // guarded by mu
}

func (s *fileSink) Info(f string, a ...any)  { s.write("INFO", f, a...) }
func (s *fileSink) Warnf(f string, a ...any) { s.write("WARN", f, a...) }
func (s *fileSink) write(level, f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	fmt.Fprintf(s.w, "%s %-4s %s\n", time.Now().Format("2006-01-02T15:04:05.000"), level, fmt.Sprintf(f, a...))
}

func (s *fileSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	_ = s.w.Close()
}
