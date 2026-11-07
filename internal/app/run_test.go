package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nlm/poly/internal/apierr"
	"github.com/nlm/poly/internal/config"
	"github.com/nlm/poly/internal/convo"
	"github.com/nlm/poly/internal/log"
	"github.com/nlm/poly/internal/stt"
	"github.com/nlm/poly/internal/translate"
	"github.com/nlm/poly/internal/usage"
)

const marker = "SECRET-PHRASE-42"

// bufSink records every log line.
type bufSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *bufSink) Info(format string, args ...any)  { s.write("INFO", format, args...) }
func (s *bufSink) Warnf(format string, args ...any) { s.write("WARN", format, args...) }
func (s *bufSink) write(level, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.buf, "%s %s\n", level, fmt.Sprintf(format, args...))
}
func (s *bufSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type fakeResolver struct{}

func (fakeResolver) Resolve(ctx context.Context, wav []byte) (stt.Resolved, error) {
	return stt.Resolved{Lang: "fr", Text: marker}, nil
}
func (f fakeResolver) ResolveLocked(ctx context.Context, wav []byte, lang string) (stt.Resolved, error) {
	return f.Resolve(ctx, wav)
}
func (fakeResolver) DetectTyped(string) string { return "fr" }

type fakeTranslator struct{}

func (fakeTranslator) Translate(ctx context.Context, text, from, to string, h []translate.Pair) (string, error) {
	return marker + " (translated)", nil
}

// fakeDetector reports speech starting on the first chunk and ending on the
// fourth, so one utterance is delivered.
type fakeDetector struct {
	n        int
	speaking bool
}

func (d *fakeDetector) Feed([]byte) (started, ended bool) {
	d.n++
	switch d.n {
	case 1:
		d.speaking = true
		return true, false
	case 4:
		d.speaking = false
		return false, true
	}
	return false, false
}
func (d *fakeDetector) Speaking() bool         { return d.speaking }
func (d *fakeDetector) DetectionLabel() string { return "fake" }
func (d *fakeDetector) Reset()                 { d.speaking = false }

// fakeUI is a terminal-less UI: Run calls during (if set), submits typed
// text, waits until wantDone rows (default 2: typed + spoken) are translated,
// then returns. With manual set, Run returns right after during instead.
type fakeUI struct {
	during   func()
	after    func() // called as Run returns
	wantDone int
	manual   bool
	mu       sync.Mutex
	rows     []convo.Row
	statuses []string
	notices  []string
	usage    []string
	submit   func(string)
	cancel   func()
	stop     chan struct{}
	once     sync.Once
}

func newFakeUI() *fakeUI { return &fakeUI{stop: make(chan struct{})} }

func (u *fakeUI) SetRows(r []convo.Row) {
	u.mu.Lock()
	u.rows = r
	u.mu.Unlock()
}
func (u *fakeUI) SetStatus(level float64, state string, n int, muted bool) {
	u.mu.Lock()
	u.statuses = append(u.statuses, state)
	u.mu.Unlock()
}
func (u *fakeUI) SetNotice(n string) {
	u.mu.Lock()
	u.notices = append(u.notices, n)
	u.mu.Unlock()
}
func (u *fakeUI) SetUsage(text string) {
	u.mu.Lock()
	u.usage = append(u.usage, text)
	u.mu.Unlock()
}
func (u *fakeUI) OnSubmit(f func(string)) { u.submit = f }
func (u *fakeUI) OnCancel(f func())       { u.cancel = f }
func (u *fakeUI) OnMuteToggle(func())     {}
func (u *fakeUI) Stop()                   { u.once.Do(func() { close(u.stop) }) }
func (u *fakeUI) Run() error {
	if u.after != nil {
		defer u.after()
	}
	if u.during != nil {
		u.during()
	}
	if u.manual {
		return nil
	}
	want := u.wantDone
	if want == 0 {
		want = 2
	}
	u.submit("typed " + marker)
	deadline := time.After(5 * time.Second)
	for {
		u.mu.Lock()
		done := 0
		for _, r := range u.rows {
			if r.Status == convo.Done {
				done++
			}
		}
		u.mu.Unlock()
		if done >= want {
			return nil
		}
		select {
		case <-deadline:
			return fmt.Errorf("timed out; rows=%+v", u.rows)
		case <-u.stop:
			return nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func testConfig() config.Config {
	cfg := config.Defaults()
	cfg.APIKey = "test-key"
	cfg.LivePreview = false // tests opt in with liveConfig
	return cfg
}

func TestNoConversationTextInLog(t *testing.T) {
	sink := &bufSink{}
	chunks := make(chan []byte, 8)
	loud := bytes.Repeat([]byte{0x00, 0x40}, 2048) // 2048 samples of 16384
	for i := 0; i < 6; i++ {
		chunks <- loud
	}
	// Closed up front so the Listener drains these chunks and then stops; Run
	// sees the stream end while the UI is still up and reports "microphone
	// stream ended" (a real mic only closes on shutdown or failure).
	close(chunks)
	u := newFakeUI()
	err := Run(context.Background(), testConfig(), Deps{
		Resolver:   fakeResolver{},
		Translator: fakeTranslator{},
		UI:         u,
		LogSink:    sink,
		Stderr:     io.Discard,
		Audio: &Audio{
			Chunks:   chunks,
			Detector: &fakeDetector{},
			Probs: func(pcm []byte) ([]float32, error) {
				p := make([]float32, len(pcm)/2/3/512)
				for i := range p {
					p[i] = 0.9
				}
				return p, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u.mu.Lock()
	rows := u.rows
	u.mu.Unlock()
	var spoken, typed bool
	for _, r := range rows {
		if r.Original == marker {
			spoken = true
		}
		if r.Original == "typed "+marker {
			typed = true
		}
	}
	if !spoken || !typed {
		t.Fatalf("expected spoken and typed rows, got %+v", rows)
	}
	logs := sink.String()
	if logs == "" {
		t.Fatal("expected some log output (timings/states)")
	}
	t.Logf("log:\n%s", logs)
	if strings.Contains(logs, marker) {
		t.Fatalf("conversation text leaked into log:\n%s", logs)
	}
}

func TestStartupFailsWithoutKey(t *testing.T) {
	cfg := testConfig()
	cfg.APIKey = ""
	cfg.MicDevice = "off"
	err := Run(context.Background(), cfg, Deps{UI: newFakeUI(), LogSink: &bufSink{}, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("want error mentioning OPENAI_API_KEY, got %v", err)
	}
}

func TestMicOffTypedInput(t *testing.T) {
	cfg := testConfig()
	cfg.MicDevice = "off"
	cfg.SileroModel = "" // nothing audio-related may be needed
	u := newFakeUI()
	u.wantDone = 1
	err := Run(context.Background(), cfg, Deps{
		Resolver: fakeResolver{}, Translator: fakeTranslator{}, UI: u,
		LogSink: &bufSink{}, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.rows) != 1 || u.rows[0].Status != convo.Done || u.rows[0].Original != "typed "+marker {
		t.Fatalf("rows = %+v", u.rows)
	}
	if len(u.statuses) == 0 {
		t.Fatal("no status pushed")
	}
	for _, st := range u.statuses {
		if st != "mic off" {
			t.Fatalf("status %q, want only \"mic off\" (all: %v)", st, u.statuses)
		}
	}
}

// syncBuf is a goroutine-safe bytes.Buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestNothingOnStderrWhileUIRuns(t *testing.T) {
	var stderr syncBuf
	sink := &bufSink{}
	chunks := make(chan []byte)
	u := newFakeUI()
	u.wantDone = 1
	u.during = func() { log.Warnf("warning-while-ui-runs") }
	u.after = func() { close(chunks) } // the real mic closes on shutdown too
	err := Run(context.Background(), testConfig(), Deps{
		Resolver: fakeResolver{}, Translator: fakeTranslator{}, UI: u,
		LogSink: sink, Stderr: &stderr,
		// The fake detector can't measure speech, so Run warns at startup
		// ("vad_min_speech_ms ignored"): proof that startup warnings do reach
		// Stderr before the UI.
		Audio: &Audio{Chunks: chunks, Detector: &fakeDetector{},
			Probs: func([]byte) ([]float32, error) { return nil, nil }},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "vad_min_speech_ms") {
		t.Fatalf("startup warning missing from stderr: %q", out)
	}
	if strings.Contains(out, "warning-while-ui-runs") || strings.Contains(out, marker) {
		t.Fatalf("stderr written while the UI ran: %q", out)
	}
	if !strings.Contains(sink.String(), "warning-while-ui-runs") {
		t.Fatalf("warning during UI should reach the log sink: %q", sink.String())
	}
}

// A late delivery of utterance N must not clear "hearing" for utterance N+1
// that the Listener already started.
func TestHearingSurvivesLateDelivery(t *testing.T) {
	u := newFakeUI()
	st := &status{ui: u, log: convo.New([2]string{"fr", "en"}, fakeResolver{}, fakeTranslator{}, 0, nil)}
	last := func() string {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.statuses[len(u.statuses)-1]
	}
	st.speechStarted() // utterance 1
	st.speechStarted() // utterance 2 starts before delivery 1 is received
	st.speechEnded()   // delivery of utterance 1
	if got := last(); got != "hearing" {
		t.Fatalf("state %q, want hearing", got)
	}
	st.speechEnded() // utterance 2 delivered
	if got := last(); got != "listening" {
		t.Fatalf("state %q, want listening", got)
	}
}

// blockingUI's first SetRows blocks until release is closed; it records the
// last rows it was given.
type blockingUI struct {
	fakeUI
	first   chan struct{} // closed when the first SetRows has started
	release chan struct{}
	calls   int
	once    sync.Once
}

func (u *blockingUI) SetRows(r []convo.Row) {
	u.mu.Lock()
	u.calls++
	n := u.calls
	u.mu.Unlock()
	if n == 1 {
		close(u.first)
		<-u.release
	}
	u.mu.Lock()
	u.rows = r
	u.mu.Unlock()
}

func (u *blockingUI) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// gateTranslator returns only once gate is closed.
type gateTranslator struct{ gate chan struct{} }

func (g gateTranslator) Translate(ctx context.Context, text, from, to string, h []translate.Pair) (string, error) {
	<-g.gate
	return "done", nil
}

// An older rows snapshot delivered late must not overwrite a newer one: the
// typed row's "translating" snapshot is held inside a blocked SetRows while
// its translation finishes and publishes "done"; the UI must end on "done".
func TestRowsNotOverwrittenByStaleSnapshot(t *testing.T) {
	u := &blockingUI{fakeUI: *newFakeUI(), first: make(chan struct{}), release: make(chan struct{})}
	st := &status{}
	rp := &rowPusher{ui: u, st: st, outcomes: &outcomeLog{seen: map[uint64]convo.Status{}}}
	gate := make(chan struct{})
	cl := convo.New([2]string{"fr", "en"}, fakeResolver{}, gateTranslator{gate}, 0, rp.changed)
	rp.log = cl
	st.ui, st.log = u, cl

	submitted := make(chan struct{})
	go func() { cl.SubmitText("bonjour"); close(submitted) }()
	<-u.first // snapshot {Original} is now held inside SetRows
	close(gate)
	// Wait for the translation to land in the log.
	waitUntil(t, func() bool { r := cl.Rows(); return len(r) == 1 && r[0].Status == convo.Done })
	// Without serialisation the worker's SetRows({Done}) completes now; give
	// it the chance to (with the fix it is parked on the pusher's lock).
	deadline := time.Now().Add(200 * time.Millisecond)
	for u.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(u.release)
	<-submitted
	waitUntil(t, func() bool { return u.callCount() >= 2 && cl.InFlight() == 0 })
	// Let any in-flight onChange finish publishing.
	rp.mu.Lock()
	rp.mu.Unlock()
	u.mu.Lock()
	got := u.rows
	u.mu.Unlock()
	want := cl.Rows()
	if len(got) != len(want) || got[0].Status != want[0].Status || got[0].Translation != want[0].Translation {
		t.Fatalf("UI shows stale rows %+v, log has %+v", got, want)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartupNoticesReachUI(t *testing.T) {
	defer func(d time.Duration) { noticeShown = d }(noticeShown)
	noticeShown = 20 * time.Millisecond
	chunks := make(chan []byte)
	u := newFakeUI()
	u.wantDone = 1
	u.during = func() { time.Sleep(200 * time.Millisecond) } // let the notice expire
	u.after = func() { close(chunks) }
	err := Run(context.Background(), testConfig(), Deps{
		Resolver: fakeResolver{}, Translator: fakeTranslator{}, UI: u,
		LogSink: &bufSink{}, Stderr: io.Discard,
		Audio: &Audio{Chunks: chunks, Detector: &fakeDetector{},
			Probs:   func([]byte) ([]float32, error) { return nil, nil },
			Notices: []string{"smart turn off", "denoise off"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.notices) != 2 || u.notices[0] != "smart turn off · denoise off" || u.notices[1] != "" {
		t.Fatalf("notices = %q, want set then cleared", u.notices)
	}
}

func (u *fakeUI) snapshot() []convo.Row {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rows
}

func (u *fakeUI) lastUsage() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.usage) == 0 {
		return ""
	}
	return u.usage[len(u.usage)-1]
}

func TestUsageShownInStatus(t *testing.T) {
	for _, priced := range []bool{false, true} {
		cfg := testConfig()
		cfg.MicDevice = "off"
		if priced {
			a, in, out := 0.003, 0.15, 0.35
			cfg.PriceAudioPerMin, cfg.PriceInPerMTok, cfg.PriceOutPerMTok = &a, &in, &out
		}
		m := usage.NewMeter(nil)
		u := newFakeUI()
		u.manual = true
		var first string
		u.during = func() {
			first = u.lastUsage()
			m.AddTokens(600, 250)
		}
		err := Run(context.Background(), cfg, Deps{
			Resolver: fakeResolver{}, Translator: fakeTranslator{}, UI: u, Meter: m,
			LogSink: &bufSink{}, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := u.lastUsage()
		if !priced {
			if first != "0 tok · 0s audio" {
				t.Fatalf("usage before Run = %q, want the zero counter", first)
			}
			if got != "850 tok · 0s audio" {
				t.Fatalf("usage = %q", got)
			}
			continue
		}
		if !strings.HasPrefix(got, "≈ ") || !strings.HasSuffix(got, "850 tok · 0s audio") {
			t.Fatalf("priced usage = %q", got)
		}
	}
}

// countingResolver counts calls and returns the fake transcription.
type countingResolver struct {
	mu           sync.Mutex
	full, locked int
}

func (r *countingResolver) Resolve(ctx context.Context, wav []byte) (stt.Resolved, error) {
	r.mu.Lock()
	r.full++
	r.mu.Unlock()
	return stt.Resolved{Lang: "fr", Text: marker}, nil
}
func (r *countingResolver) ResolveLocked(ctx context.Context, wav []byte, lang string) (stt.Resolved, error) {
	r.mu.Lock()
	r.locked++
	r.mu.Unlock()
	return stt.Resolved{Lang: "fr", Text: marker}, nil
}
func (*countingResolver) DetectTyped(string) string { return "fr" }

func TestLivePreviewOffUsesSubmitAudio(t *testing.T) {
	chunks := make(chan []byte, 8)
	loud := bytes.Repeat([]byte{0x00, 0x40}, 2048)
	for i := 0; i < 6; i++ {
		chunks <- loud
	}
	close(chunks)
	r := &countingResolver{}
	u := newFakeUI()
	cfg := testConfig()
	cfg.LivePreview = false
	err := Run(context.Background(), cfg, Deps{
		Resolver: r, Translator: fakeTranslator{}, UI: u,
		LogSink: &bufSink{}, Stderr: io.Discard,
		Audio: &Audio{Chunks: chunks, Detector: &fakeDetector{}, Probs: allVoiced},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked != 0 || r.full != 1 {
		t.Fatalf("Resolve calls %d, ResolveLocked calls %d; want 1 and 0", r.full, r.locked)
	}
	var spoken int
	for _, row := range u.snapshot() {
		if row.Original == marker {
			spoken++
		}
	}
	if spoken != 1 {
		t.Fatalf("want one spoken row, got %+v", u.snapshot())
	}
}

// allVoiced gives every trim window a high speech probability.
func allVoiced(pcm []byte) ([]float32, error) {
	p := make([]float32, len(pcm)/2/3/512)
	for i := range p {
		p[i] = 0.9
	}
	return p, nil
}

// gateDetector starts speech on the first chunk and ends it on the first
// chunk after end() while still speaking. Reset (DropCurrent) stops it for
// good: no new utterance starts.
type gateDetector struct {
	mu       sync.Mutex
	n        int
	speaking bool
	ending   bool
}

func (d *gateDetector) Feed([]byte) (started, ended bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	switch {
	case d.n == 1:
		d.speaking = true
		return true, false
	case d.speaking && d.ending:
		d.speaking = false
		return false, true
	}
	return false, false
}
func (d *gateDetector) Speaking() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.speaking
}
func (d *gateDetector) DetectionLabel() string { return "gate" }
func (d *gateDetector) Reset() {
	d.mu.Lock()
	d.speaking = false
	d.mu.Unlock()
}
func (d *gateDetector) end() {
	d.mu.Lock()
	d.ending = true
	d.mu.Unlock()
}

// scriptedResolver answers ResolveLocked with script(call index, ended),
// recording each call's WAV size and whether the turn had been ended.
type scriptedResolver struct {
	script func(n int, ended bool) stt.Resolved
	mu     sync.Mutex
	ended  bool
	calls  []lockedCall
	full   int
}

type lockedCall struct {
	wavLen int
	ended  bool
}

func (r *scriptedResolver) Resolve(ctx context.Context, wav []byte) (stt.Resolved, error) {
	r.mu.Lock()
	r.full++
	r.mu.Unlock()
	return stt.Resolved{Lang: "fr", Text: marker}, nil
}
func (r *scriptedResolver) ResolveLocked(ctx context.Context, wav []byte, lang string) (stt.Resolved, error) {
	r.mu.Lock()
	n, ended := len(r.calls), r.ended
	r.calls = append(r.calls, lockedCall{len(wav), ended})
	r.mu.Unlock()
	return r.script(n, ended), nil
}
func (*scriptedResolver) DetectTyped(string) string { return "fr" }
func (r *scriptedResolver) setEnded() {
	r.mu.Lock()
	r.ended = true
	r.mu.Unlock()
}

// chunk100ms is 100 ms of loud 48 kHz mono s16le audio.
var chunk100ms = bytes.Repeat([]byte{0x00, 0x40}, 4800)

func liveConfig() config.Config {
	cfg := testConfig()
	cfg.LivePreview = true
	return cfg
}

func TestLivePreviewCommitsBeforeEndOfTurn(t *testing.T) {
	r := &scriptedResolver{script: func(n int, ended bool) stt.Resolved {
		if n == 0 && !ended {
			return stt.Resolved{Lang: "fr", Text: "Un. Deux", Segments: []stt.Segment{
				{Start: 0, End: 0.5, Text: "Un."}, {Start: 0.5, End: 1, Text: "Deux"}}}
		}
		return stt.Resolved{Lang: "fr", Text: "Deux.", Segments: []stt.Segment{{Start: 0, End: 1, Text: "Deux."}}}
	}}
	det := &gateDetector{}
	chunks := make(chan []byte)
	var probed int
	var probedMu sync.Mutex
	probs := func(pcm []byte) ([]float32, error) {
		probedMu.Lock()
		probed++
		probedMu.Unlock()
		return allVoiced(pcm)
	}
	u := newFakeUI()
	u.manual = true
	committedEarly := false
	u.during = func() {
		for i := 0; i < 40; i++ { // 4 s of speech, turn still open
			chunks <- chunk100ms
		}
		waitUntil(t, func() bool {
			for _, row := range u.snapshot() {
				if row.Original == "Un." && row.Status == convo.Done {
					return true
				}
			}
			return false
		})
		probedMu.Lock()
		committedEarly = probed == 0 // nothing delivered yet
		probedMu.Unlock()
		r.setEnded()
		det.end()
		chunks <- chunk100ms // ends the turn
		waitUntil(t, func() bool {
			rows := u.snapshot()
			return len(rows) == 2 && rows[1].Status == convo.Done
		})
		close(chunks)
	}
	err := Run(context.Background(), liveConfig(), Deps{
		Resolver: r, Translator: fakeTranslator{}, UI: u,
		LogSink: &bufSink{}, Stderr: io.Discard,
		Audio: &Audio{Chunks: chunks, Detector: det, Probs: probs},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !committedEarly {
		t.Fatal("the committed row appeared only after the turn ended")
	}
	rows := u.snapshot()
	if len(rows) != 2 || rows[0].Original != "Un." || rows[1].Original != "Deux." || rows[1].Status != convo.Done {
		t.Fatalf("rows = %+v", rows)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full != 0 {
		t.Fatalf("SubmitAudio path used (%d full resolves)", r.full)
	}
	whole := (41*len(chunk100ms))/3 + 44 // the whole utterance as 16 kHz WAV
	var final bool
	for _, c := range r.calls {
		if !c.ended {
			continue
		}
		final = true
		if c.wavLen > whole-48000/3 { // 0.5 s committed early
			t.Fatalf("final pass sent %d bytes: not just the uncommitted rest (whole: %d)", c.wavLen, whole)
		}
	}
	if !final {
		t.Fatal("no ResolveLocked call after the turn ended (FinishLive)")
	}
}

func TestEscapeDropsLiveTurn(t *testing.T) {
	r := &scriptedResolver{script: func(int, bool) stt.Resolved {
		return stt.Resolved{Lang: "fr", Text: "Bonjour", Segments: []stt.Segment{{Start: 0, End: 1, Text: "Bonjour"}}}
	}}
	det := &gateDetector{}
	chunks := make(chan []byte)
	var probed int
	var probedMu sync.Mutex
	probs := func(pcm []byte) ([]float32, error) {
		probedMu.Lock()
		probed++
		probedMu.Unlock()
		return allVoiced(pcm)
	}
	u := newFakeUI()
	u.manual = true
	u.during = func() {
		for i := 0; i < 15; i++ {
			chunks <- chunk100ms
		}
		waitUntil(t, func() bool {
			rows := u.snapshot()
			return len(rows) == 1 && rows[0].Status == convo.Live && rows[0].Original == "Bonjour"
		})
		u.cancel()
		waitUntil(t, func() bool { return len(u.snapshot()) == 0 })
		det.end()
		for i := 0; i < 15; i++ { // speech after Escape: no turn to end
			chunks <- chunk100ms
		}
		close(chunks)
		time.Sleep(50 * time.Millisecond) // let a wrong delivery surface
	}
	err := Run(context.Background(), liveConfig(), Deps{
		Resolver: r, Translator: fakeTranslator{}, UI: u,
		LogSink: &bufSink{}, Stderr: io.Discard,
		Audio: &Audio{Chunks: chunks, Detector: det, Probs: probs},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rows := u.snapshot(); len(rows) != 0 {
		t.Fatalf("rows after Escape = %+v", rows)
	}
	probedMu.Lock()
	defer probedMu.Unlock()
	if probed != 0 {
		t.Fatalf("the dropped turn was delivered (%d trims)", probed)
	}
}

// blockingResolver blocks every call until its context is done.
type blockingResolver struct{ called chan struct{} }

func (r *blockingResolver) Resolve(ctx context.Context, wav []byte) (stt.Resolved, error) {
	<-ctx.Done()
	return stt.Resolved{}, ctx.Err()
}
func (r *blockingResolver) ResolveLocked(ctx context.Context, wav []byte, lang string) (stt.Resolved, error) {
	select {
	case r.called <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return stt.Resolved{}, ctx.Err()
}
func (*blockingResolver) DetectTyped(string) string { return "fr" }

// Quitting while a live refresh is blocked must not wait for its timeout.
func TestQuitWithBlockedLiveRefresh(t *testing.T) {
	r := &blockingResolver{called: make(chan struct{}, 1)}
	chunks := make(chan []byte)
	u := newFakeUI()
	u.manual = true
	u.during = func() {
		for i := 0; i < 15; i++ {
			chunks <- chunk100ms
		}
		select {
		case <-r.called:
		case <-time.After(5 * time.Second):
			t.Error("no live refresh started")
		}
		close(chunks)
	}
	var quit time.Time
	u.after = func() { quit = time.Now() }
	err := Run(context.Background(), liveConfig(), Deps{
		Resolver: r, Translator: fakeTranslator{}, UI: u,
		LogSink: &bufSink{}, Stderr: io.Discard,
		Audio: &Audio{Chunks: chunks, Detector: &gateDetector{}, Probs: allVoiced},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(quit); d > 3*time.Second {
		t.Fatalf("Run returned %v after the UI quit, want about a second at most", d)
	}
}

type errResolver struct {
	fakeResolver
	res stt.Resolved
	err error
}

func (r errResolver) ResolveLocked(ctx context.Context, wav []byte, lang string) (stt.Resolved, error) {
	return r.res, r.err
}

// Each live refresh log line says how it ended, never with any text.
func TestRefreshLogOutcome(t *testing.T) {
	cases := []struct {
		res  stt.Resolved
		err  error
		want string
	}{
		{stt.Resolved{Lang: "fr", Text: marker}, nil, "ok"},
		{stt.Resolved{}, fmt.Errorf("resolve: %w", stt.ErrNoise), "noise"},
		{stt.Resolved{}, fmt.Errorf("stt: %w", &apierr.StatusError{Op: "transcription", Code: 503, Status: "503 Service Unavailable"}), "503 Service Unavailable"},
		{stt.Resolved{}, fmt.Errorf("post: %w", context.DeadlineExceeded), "timeout"},
	}
	for _, c := range cases {
		sink := &bufSink{}
		log.SetSink(sink)
		_, _ = refreshLogger{errResolver{res: c.res, err: c.err}}.ResolveLocked(context.Background(), make([]byte, 44+32000), "")
		log.SetSink(discardSink{})
		got := sink.String()
		if !strings.Contains(got, "live refresh: "+c.want+",") {
			t.Errorf("err %v: log %q lacks outcome %q", c.err, got, c.want)
		}
		if strings.Contains(got, marker) {
			t.Errorf("log leaks text: %q", got)
		}
	}
}
