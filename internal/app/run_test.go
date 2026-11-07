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

	"github.com/nlm/poly/internal/config"
	"github.com/nlm/poly/internal/convo"
	"github.com/nlm/poly/internal/log"
	"github.com/nlm/poly/internal/stt"
	"github.com/nlm/poly/internal/translate"
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
// then returns.
type fakeUI struct {
	during   func()
	after    func() // called as Run returns
	wantDone int
	mu       sync.Mutex
	rows     []convo.Row
	statuses []string
	notices  []string
	submit   func(string)
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
func (u *fakeUI) OnSubmit(f func(string)) { u.submit = f }
func (u *fakeUI) OnCancel(func())         {}
func (u *fakeUI) OnMuteToggle(func())     {}
func (u *fakeUI) Stop()                   { u.once.Do(func() { close(u.stop) }) }
func (u *fakeUI) Run() error {
	if u.after != nil {
		defer u.after()
	}
	if u.during != nil {
		u.during()
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
