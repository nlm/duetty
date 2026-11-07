package convo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nlm/poly/internal/apierr"
	"github.com/nlm/poly/internal/stt"
	"github.com/nlm/poly/internal/translate"
)

// ---- fakes controlled by channels ----

type resReply struct {
	res stt.Resolved
	err error
}

type resCall struct {
	ctx   context.Context
	wav   string
	reply chan resReply
}

// fakeResolver hands every Resolve call to the test via calls and blocks
// until the test replies. It deliberately ignores ctx unless honorCtx is
// set, so late results after cancellation really reach the Log.
type fakeResolver struct {
	calls    chan resCall
	typed    string
	honorCtx bool
}

func (f *fakeResolver) Resolve(ctx context.Context, wav []byte) (stt.Resolved, error) {
	c := resCall{ctx: ctx, wav: string(wav), reply: make(chan resReply, 1)}
	f.calls <- c
	if f.honorCtx {
		select {
		case r := <-c.reply:
			return r.res, r.err
		case <-ctx.Done():
			return stt.Resolved{}, ctx.Err()
		}
	}
	r := <-c.reply
	return r.res, r.err
}

func (f *fakeResolver) DetectTyped(string) string { return f.typed }

type trReply struct {
	text string
	err  error
}

type trCall struct {
	ctx            context.Context
	text, from, to string
	history        []translate.Pair
	reply          chan trReply
}

type fakeTranslator struct{ calls chan trCall }

func (f *fakeTranslator) Translate(ctx context.Context, text, from, to string, history []translate.Pair) (string, error) {
	c := trCall{ctx: ctx, text: text, from: from, to: to, history: history, reply: make(chan trReply, 1)}
	f.calls <- c
	r := <-c.reply
	return r.text, r.err
}

type harness struct {
	t       *testing.T
	log     *Log
	res     *fakeResolver
	tr      *fakeTranslator
	changed chan struct{}
}

func newHarness(t *testing.T, contextN int) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		res:     &fakeResolver{calls: make(chan resCall, 16), typed: "fr"},
		tr:      &fakeTranslator{calls: make(chan trCall, 16)},
		changed: make(chan struct{}, 1),
	}
	h.log = New([2]string{"fr", "en"}, h.res, h.tr, contextN, func() {
		// Calling back into the Log must not deadlock (onChange runs unlocked).
		_ = h.log.Rows()
		_ = h.log.InFlight()
		select {
		case h.changed <- struct{}{}:
		default:
		}
	})
	t.Cleanup(h.idle) // every worker goroutine must finish
	return h
}

const wait = time.Second

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatal("timed out waiting for call")
		panic("unreachable")
	}
}

// resolveCalls receives n Resolve calls and indexes them by wav payload.
func (h *harness) resolveCalls(n int) map[string]resCall {
	h.t.Helper()
	m := map[string]resCall{}
	for range n {
		c := recv(h.t, h.res.calls)
		m[c.wav] = c
	}
	return m
}

// idle waits (bounded) until every worker goroutine of the Log has returned.
func (h *harness) idle() {
	h.t.Helper()
	done := make(chan struct{})
	go func() { h.log.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		h.t.Fatal("timed out waiting for Log workers to finish (a request is still outstanding)")
	}
}

// waitFor blocks until cond(Rows()) holds, waking on onChange, bounded by 1 s.
func (h *harness) waitFor(desc string, cond func([]Row) bool) []Row {
	h.t.Helper()
	deadline := time.After(wait)
	for {
		rows := h.log.Rows()
		if cond(rows) {
			return rows
		}
		select {
		case <-h.changed:
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s; rows = %s", desc, dump(rows))
		}
	}
}

func dump(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "{#%d %v %s %q→%q err=%q} ", r.Seq, r.Status, r.Lang, r.Original, r.Translation, r.Err)
	}
	return b.String()
}

func originals(rows []Row) []string {
	var s []string
	for _, r := range rows {
		s = append(s, r.Original)
	}
	return s
}

func eq(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func resolved(lang, text string) resReply { return resReply{res: stt.Resolved{Lang: lang, Text: text}} }

// ---- tests ----

// Review Focus 2: rows appear in capture order, not completion order.
func TestOrderByCaptureNotCompletion(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitAudio([]byte("A"))
	h.log.SubmitAudio([]byte("B"))
	calls := h.resolveCalls(2)
	if got := h.log.InFlight(); got != 2 {
		t.Errorf("InFlight = %d want 2", got)
	}
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("pending rows must be hidden: %s", dump(rows))
	}

	calls["B"].reply <- resolved("en", "bee")
	rows := h.waitFor("B visible", func(r []Row) bool { return len(r) == 1 })
	if rows[0].Original != "bee" || rows[0].Seq != 2 {
		t.Fatalf("rows = %s", dump(rows))
	}

	calls["A"].reply <- resolved("fr", "ah")
	rows = h.waitFor("A inserted above B", func(r []Row) bool { return len(r) == 2 })
	if !eq(originals(rows), []string{"ah", "bee"}) || rows[0].Seq >= rows[1].Seq {
		t.Fatalf("order = %s", dump(rows))
	}
	if rows[0].At.After(rows[1].At) {
		t.Errorf("At must be capture time: %v after %v", rows[0].At, rows[1].At)
	}

	// finish translations in reverse order too
	tc := map[string]trCall{}
	for range 2 {
		c := recv(t, h.tr.calls)
		tc[c.text] = c
	}
	tc["bee"].reply <- trReply{text: "abeille"}
	tc["ah"].reply <- trReply{text: "oh"}
	rows = h.waitFor("both done", func(r []Row) bool {
		return len(r) == 2 && r[0].Status == Done && r[1].Status == Done
	})
	if rows[0].Translation != "oh" || rows[1].Translation != "abeille" {
		t.Errorf("rows = %s", dump(rows))
	}
	if h.log.InFlight() != 0 {
		t.Errorf("InFlight = %d", h.log.InFlight())
	}
}

func TestOriginalShownBeforeTranslation(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitAudio([]byte("A"))
	recv(t, h.res.calls).reply <- resolved("fr", "Bonjour")
	tc := recv(t, h.tr.calls)
	rows := h.waitFor("original", func(r []Row) bool { return len(r) == 1 })
	if r := rows[0]; r.Status != Original || r.Translation != "" || r.Lang != "fr" || r.Original != "Bonjour" {
		t.Fatalf("row = %s", dump(rows))
	}
	if h.log.InFlight() != 1 {
		t.Errorf("InFlight = %d want 1", h.log.InFlight())
	}
	if tc.from != "fr" || tc.to != "en" || tc.text != "Bonjour" {
		t.Errorf("translate call = %q %s→%s", tc.text, tc.from, tc.to)
	}
	tc.reply <- trReply{text: "Hello"}
	rows = h.waitFor("done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })
	if rows[0].Translation != "Hello" || rows[0].Err != "" {
		t.Errorf("row = %s", dump(rows))
	}
}

func TestEnglishSourceTranslatesToFrench(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitAudio([]byte("A"))
	recv(t, h.res.calls).reply <- resolved("en", "Hello")
	tc := recv(t, h.tr.calls)
	if tc.from != "en" || tc.to != "fr" {
		t.Errorf("direction %s→%s", tc.from, tc.to)
	}
	tc.reply <- trReply{text: "Bonjour"}
	h.waitFor("done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })
}

func TestNoiseNeverVisible(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitAudio([]byte("A"))
	recv(t, h.res.calls).reply <- resReply{err: fmt.Errorf("resolve: %w", stt.ErrNoise)}
	h.idle()
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Errorf("rows = %s", dump(rows))
	}
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d", n)
	}
}

// Review Focus 1: API errors become error rows; later utterances still work.
func TestTranscriptionErrorRow(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"5xx", fmt.Errorf("resolve: %w", &apierr.StatusError{Op: "transcription", Code: 503, Status: "503 Service Unavailable"}), "transcription failed: 503 Service Unavailable"},
		{"401", &apierr.StatusError{Op: "transcription", Code: 401, Status: "401 Unauthorized"}, "transcription failed: 401 Unauthorized"},
		{"deadline", context.DeadlineExceeded, "transcription failed: timeout"},
		{"network", errors.New(`transcription request: Post "https://host/p/v1/audio/transcriptions": dial tcp: connection refused`), "transcription failed: connection refused"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 0)
			h.log.SubmitAudio([]byte("A"))
			recv(t, h.res.calls).reply <- resReply{err: tt.err}
			rows := h.waitFor("failed row", func(r []Row) bool { return len(r) == 1 && r[0].Status == Failed })
			if rows[0].Err != tt.want {
				t.Errorf("Err = %q want %q", rows[0].Err, tt.want)
			}
			if !strings.HasPrefix(rows[0].Err, "transcription failed: ") {
				t.Errorf("prefix: %q", rows[0].Err)
			}
			if h.log.InFlight() != 0 {
				t.Errorf("InFlight = %d", h.log.InFlight())
			}

			// listening continues
			h.log.SubmitAudio([]byte("B"))
			recv(t, h.res.calls).reply <- resolved("fr", "encore")
			recv(t, h.tr.calls).reply <- trReply{text: "again"}
			rows = h.waitFor("second row done", func(r []Row) bool { return len(r) == 2 && r[1].Status == Done })
			if rows[0].Status != Failed || rows[1].Translation != "again" {
				t.Errorf("rows = %s", dump(rows))
			}
		})
	}
}

func TestTranslationErrorRow(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitAudio([]byte("A"))
	recv(t, h.res.calls).reply <- resolved("fr", "Bonjour")
	recv(t, h.tr.calls).reply <- trReply{err: fmt.Errorf("x: %w", &apierr.StatusError{Op: "translation", Code: 500, Status: "500 Internal Server Error"})}
	rows := h.waitFor("failed", func(r []Row) bool { return len(r) == 1 && r[0].Status == Failed })
	if r := rows[0]; r.Err != "translation failed: 500 Internal Server Error" || r.Original != "Bonjour" || r.Translation != "" {
		t.Errorf("row = %s", dump(rows))
	}

	h.log.SubmitText("encore")
	recv(t, h.tr.calls).reply <- trReply{text: "again"}
	rows = h.waitFor("next done", func(r []Row) bool { return len(r) == 2 && r[1].Status == Done })
	if rows[1].Translation != "again" {
		t.Errorf("rows = %s", dump(rows))
	}
}

// Review Focus 1 (hang): a request that never answers times out by itself.
func TestRequestTimeout(t *testing.T) {
	h := newHarness(t, 0)
	h.res.honorCtx = true
	h.log.timeout = 20 * time.Millisecond
	h.log.SubmitAudio([]byte("A"))
	c := recv(t, h.res.calls)
	if dl, ok := c.ctx.Deadline(); !ok || time.Until(dl) > time.Second {
		t.Errorf("request ctx must carry the timeout: %v %v", dl, ok)
	}
	rows := h.waitFor("timeout row", func(r []Row) bool { return len(r) == 1 && r[0].Status == Failed })
	if rows[0].Err != "transcription failed: timeout" {
		t.Errorf("Err = %q", rows[0].Err)
	}
}

func TestDefaultTimeoutIs30s(t *testing.T) {
	h := newHarness(t, 0)
	if h.log.timeout != 30*time.Second {
		t.Errorf("timeout = %v", h.log.timeout)
	}
}

// Review Focus 3: a response arriving after Escape never overwrites the row.
func TestCancelPending(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitAudio([]byte("A"))
	recv(t, h.res.calls).reply <- resolved("fr", "Bonjour")
	trA := recv(t, h.tr.calls) // A is Original, translating
	h.log.SubmitAudio([]byte("B"))
	resB := recv(t, h.res.calls) // B is Pending
	h.waitFor("A original", func(r []Row) bool { return len(r) == 1 && r[0].Status == Original })

	h.log.CancelPending()
	rows := h.log.Rows()
	if len(rows) != 1 || rows[0].Status != Cancelled || rows[0].Original != "Bonjour" {
		t.Fatalf("after cancel rows = %s", dump(rows))
	}
	if h.log.InFlight() != 0 {
		t.Errorf("InFlight = %d", h.log.InFlight())
	}
	if trA.ctx.Err() == nil || resB.ctx.Err() == nil {
		t.Error("in-flight request contexts must be cancelled")
	}

	// late results
	trA.reply <- trReply{text: "Hello"}
	resB.reply <- resolved("en", "late")
	h.idle()
	select {
	case c := <-h.tr.calls:
		t.Fatalf("cancelled row must not be translated: %q", c.text)
	default:
	}
	after := h.log.Rows()
	if fmt.Sprint(after) != fmt.Sprint(rows) {
		t.Errorf("late results changed rows:\n%s\n%s", dump(rows), dump(after))
	}

	// new submissions still work after a cancel
	h.log.SubmitAudio([]byte("C"))
	recv(t, h.res.calls).reply <- resolved("en", "next")
	recv(t, h.tr.calls).reply <- trReply{text: "suivant"}
	rows = h.waitFor("C done", func(r []Row) bool { return len(r) == 2 && r[1].Status == Done })
	if rows[0].Status != Cancelled || rows[1].Translation != "suivant" || rows[1].Seq <= rows[0].Seq {
		t.Errorf("rows = %s", dump(rows))
	}
}

func TestCancelLeavesFinishedRowsAlone(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitText("un")
	recv(t, h.tr.calls).reply <- trReply{text: "one"}
	h.log.SubmitAudio([]byte("A"))
	recv(t, h.res.calls).reply <- resReply{err: context.DeadlineExceeded}
	h.waitFor("done+failed", func(r []Row) bool { return len(r) == 2 && r[0].Status == Done && r[1].Status == Failed })
	h.log.CancelPending()
	rows := h.log.Rows()
	if rows[0].Status != Done || rows[1].Status != Failed {
		t.Errorf("rows = %s", dump(rows))
	}
}

func TestTypedText(t *testing.T) {
	h := newHarness(t, 0)
	h.res.typed = "es"
	h.log.SubmitText("  ")
	h.log.SubmitText("")
	if rows := h.log.Rows(); len(rows) != 0 || h.log.InFlight() != 0 {
		t.Fatalf("blank text must be ignored: %s", dump(rows))
	}
	h.log.SubmitText("hola")
	// visible immediately, no Pending stage
	rows := h.log.Rows()
	if len(rows) != 1 || rows[0].Status != Original || rows[0].Lang != "es" || rows[0].Original != "hola" || rows[0].Seq != 1 {
		t.Fatalf("rows = %s", dump(rows))
	}
	tc := recv(t, h.tr.calls)
	if tc.text != "hola" || tc.from != "es" {
		t.Errorf("call %q from %s", tc.text, tc.from)
	}
	tc.reply <- trReply{text: "salut"}
	h.waitFor("done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })
}

func TestContextWindow(t *testing.T) {
	h := newHarness(t, 2)
	for i, s := range []string{"un", "deux", "trois"} {
		h.log.SubmitText(s)
		c := recv(t, h.tr.calls)
		if len(c.history) != min(i, 2) {
			t.Errorf("row %d history len %d", i, len(c.history))
		}
		c.reply <- trReply{text: fmt.Sprint("t-", s)}
		h.waitFor("done", func(r []Row) bool { return len(r) == i+1 && r[i].Status == Done })
	}
	// a non-Done row is not context
	h.log.SubmitAudio([]byte("X"))
	recv(t, h.res.calls).reply <- resReply{err: context.DeadlineExceeded}
	h.waitFor("failed", func(r []Row) bool { return len(r) == 4 && r[3].Status == Failed })

	h.log.SubmitText("quatre")
	c := recv(t, h.tr.calls)
	want := []translate.Pair{
		{Lang: "fr", Original: "deux", Translation: "t-deux"},
		{Lang: "fr", Original: "trois", Translation: "t-trois"},
	}
	if fmt.Sprint(c.history) != fmt.Sprint(want) {
		t.Errorf("history = %+v want %+v", c.history, want)
	}
	c.reply <- trReply{text: "four"}
	h.waitFor("done", func(r []Row) bool { return len(r) == 5 && r[4].Status == Done })
}

// History is rows *before* the current one: a later row that finishes first
// is not context for an earlier one.
func TestContextOnlyEarlierRows(t *testing.T) {
	h := newHarness(t, 4)
	h.log.SubmitAudio([]byte("A"))
	resA := recv(t, h.res.calls)
	h.log.SubmitText("later")
	recv(t, h.tr.calls).reply <- trReply{text: "plus tard"}
	h.waitFor("later done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })
	resA.reply <- resolved("fr", "first")
	c := recv(t, h.tr.calls)
	if len(c.history) != 0 {
		t.Errorf("history = %+v", c.history)
	}
	c.reply <- trReply{text: "premier"}
	h.waitFor("done", func(r []Row) bool { return len(r) == 2 && r[0].Status == Done })
}

func TestRowsReturnsCopy(t *testing.T) {
	h := newHarness(t, 0)
	h.log.SubmitText("abc")
	rows := h.log.Rows()
	rows[0].Original = "mutated"
	if h.log.Rows()[0].Original != "abc" {
		t.Error("Rows must return a copy")
	}
	recv(t, h.tr.calls).reply <- trReply{text: "x"}
	h.waitFor("done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{Pending: "pending", Original: "original", Done: "done", Cancelled: "cancelled", Failed: "failed"} {
		if s.String() != want {
			t.Errorf("%d = %q", s, s.String())
		}
	}
}
