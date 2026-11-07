package convo

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nlm/duetty/internal/stt"
)

func liveRow(rows []Row) (Row, bool) {
	for _, r := range rows {
		if r.Status == Live {
			return r, true
		}
	}
	return Row{}, false
}

func TestLiveStatusString(t *testing.T) {
	if Live.String() != "live" {
		t.Errorf("Live = %q", Live.String())
	}
	if Live <= Failed {
		t.Error("Live must come after Failed")
	}
}

func TestLiveHiddenUntilText(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("empty live entry must be hidden: %s", dump(rows))
	}
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d want 0", n)
	}
	h.log.UpdateLive(id, "fr", "Bonjour")
	rows := h.log.Rows()
	if len(rows) != 1 || rows[0].Status != Live || rows[0].Lang != "fr" || rows[0].Original != "Bonjour" {
		t.Fatalf("rows = %s", dump(rows))
	}
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d want 0 (live entry not finishing)", n)
	}
	c := recv(t, h.tr.calls)
	if c.text != "Bonjour" || c.from != "fr" || c.to != "en" {
		t.Errorf("live translate call = %q %s→%s", c.text, c.from, c.to)
	}
	c.reply <- trReply{text: "Hello"}
	h.waitFor("live translation", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "Hello" })
}

func TestLiveTranslationLatestWins(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "a")
	first := recv(t, h.tr.calls)
	h.log.UpdateLive(id, "fr", "a b")
	h.log.UpdateLive(id, "fr", "a b c")
	select {
	case c := <-h.tr.calls:
		t.Fatalf("second live translation %q started while one is in flight", c.text)
	default:
	}
	if first.text != "a" {
		t.Fatalf("first call = %q", first.text)
	}
	first.reply <- trReply{text: "T(a)"}
	second := recv(t, h.tr.calls)
	if second.text != "a b c" {
		t.Fatalf("second call = %q want latest text", second.text)
	}
	second.reply <- trReply{text: "T(a b c)"}
	rows := h.waitFor("latest translation", func(r []Row) bool {
		return len(r) == 1 && r[0].Translation == "T(a b c)"
	})
	if rows[0].Stale || rows[0].Status != Live {
		t.Errorf("row = %s stale=%v", dump(rows), rows[0].Stale)
	}
	h.idle()
	select {
	case c := <-h.tr.calls:
		t.Fatalf("unexpected extra call %q", c.text)
	default:
	}
}

func TestLiveTranslationStaleFlag(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "a")
	c := recv(t, h.tr.calls)
	h.log.UpdateLive(id, "fr", "a b")
	c.reply <- trReply{text: "T(a)"}
	rows := h.waitFor("stale translation", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a)" })
	if !rows[0].Stale || rows[0].Original != "a b" {
		t.Errorf("row = %s stale=%v", dump(rows), rows[0].Stale)
	}
	c = recv(t, h.tr.calls)
	if c.text != "a b" {
		t.Fatalf("call = %q", c.text)
	}
	c.reply <- trReply{text: "T(a b)"}
	rows = h.waitFor("fresh translation", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a b)" })
	if rows[0].Stale {
		t.Error("Stale must clear once the current text is translated")
	}
}

func TestLiveTranslationErrorKeepsPrevious(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "a")
	recv(t, h.tr.calls).reply <- trReply{text: "T(a)"}
	h.waitFor("first", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a)" })
	h.log.UpdateLive(id, "fr", "a b")
	recv(t, h.tr.calls).reply <- trReply{err: errors.New("boom")}
	h.idle()
	rows := h.log.Rows()
	if len(rows) != 1 || rows[0].Status != Live || rows[0].Translation != "T(a)" || rows[0].Err != "" {
		t.Fatalf("rows = %s", dump(rows))
	}
}

func TestLiveSameTextNotRetranslated(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "a")
	recv(t, h.tr.calls).reply <- trReply{text: "T(a)"}
	h.waitFor("translated", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a)" })
	h.log.UpdateLive(id, "fr", "a")
	h.idle()
	select {
	case c := <-h.tr.calls:
		t.Fatalf("unchanged text translated again: %q", c.text)
	default:
	}
}

func TestCommitLiveOrdering(t *testing.T) {
	h := newHarness(t, 5)
	h.log.SubmitText("T1")
	recv(t, h.tr.calls).reply <- trReply{text: "t1"}
	h.waitFor("T1 done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })

	id := h.log.StartLive()
	h.log.SubmitText("T2")
	h.log.CommitLive(id, "fr", "Phrase un.")
	h.log.UpdateLive(id, "fr", "Phrase deux")

	calls := map[string]trCall{}
	for range 3 {
		c := recv(t, h.tr.calls)
		calls[c.text] = c
	}
	pc, ok := calls["Phrase un."]
	if !ok {
		t.Fatalf("no translation of the committed sentence: %v", calls)
	}
	if len(pc.history) != 1 || pc.history[0].Original != "T1" {
		t.Errorf("committed history = %+v want T1 only", pc.history)
	}
	if _, ok := calls["Phrase deux"]; !ok {
		t.Fatalf("no live translation of the new text: %v", calls)
	}

	rows := h.log.Rows()
	if !eq(originals(rows), []string{"T1", "Phrase un.", "T2", "Phrase deux"}) {
		t.Fatalf("order = %s", dump(rows))
	}
	if rows[1].Status != Original || rows[1].Lang != "fr" || rows[3].Status != Live || rows[3].Translation != "" {
		t.Fatalf("rows = %s", dump(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Seq >= rows[i].Seq {
			t.Fatalf("Seq not increasing: %s", dump(rows))
		}
	}

	for text, c := range calls {
		c.reply <- trReply{text: "tr " + text}
	}
	rows = h.waitFor("all translated", func(r []Row) bool {
		return len(r) == 4 && r[1].Status == Done && r[2].Status == Done && r[3].Translation != ""
	})
	if rows[1].Translation != "tr Phrase un." || rows[3].Translation != "tr Phrase deux" || rows[3].Status != Live {
		t.Errorf("rows = %s", dump(rows))
	}
}

func TestCommitLiveDiscardsInFlightLiveTranslation(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "Phrase un. Deu")
	old := recv(t, h.tr.calls)
	h.log.CommitLive(id, "fr", "Phrase un.")
	commit := recv(t, h.tr.calls)
	if commit.text != "Phrase un." {
		t.Fatalf("commit call = %q", commit.text)
	}
	h.log.UpdateLive(id, "fr", "Deux")
	old.reply <- trReply{text: "OLD"}
	next := recv(t, h.tr.calls)
	if next.text != "Deux" {
		t.Fatalf("next live call = %q", next.text)
	}
	rows := h.log.Rows()
	if r, ok := liveRow(rows); !ok || r.Translation != "" || r.Original != "Deux" {
		t.Fatalf("late live translation of committed text applied: %s", dump(rows))
	}
	commit.reply <- trReply{text: "Sentence one."}
	next.reply <- trReply{text: "Two"}
	h.waitFor("done", func(r []Row) bool {
		return len(r) == 2 && r[0].Status == Done && r[1].Translation == "Two"
	})
}

func TestFinishLiveBecomesRow(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "Fi")
	recv(t, h.tr.calls).reply <- trReply{text: "En"}
	h.waitFor("live translated", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "En" })

	h.log.FinishLive(id, []byte("W"), "")
	c := recv(t, h.res.calls)
	if c.wav != "W" {
		t.Errorf("wav = %q", c.wav)
	}
	rows := h.log.Rows()
	if len(rows) != 1 || rows[0].Status != Live || rows[0].Original != "Fi" || rows[0].Translation != "En" {
		t.Fatalf("while finishing, rows = %s", dump(rows))
	}
	if n := h.log.InFlight(); n != 1 {
		t.Errorf("InFlight = %d want 1", n)
	}
	h.log.UpdateLive(id, "fr", "ignored")
	if r := h.log.Rows(); r[0].Original != "Fi" {
		t.Errorf("UpdateLive applied while finishing: %s", dump(r))
	}

	c.reply <- resolved("fr", "Fin.")
	tc := recv(t, h.tr.calls)
	if tc.text != "Fin." {
		t.Fatalf("translate call = %q", tc.text)
	}
	rows = h.waitFor("original", func(r []Row) bool { return len(r) == 1 && r[0].Status == Original })
	if rows[0].Original != "Fin." || rows[0].Translation != "" || rows[0].Stale {
		t.Errorf("row = %s", dump(rows))
	}
	tc.reply <- trReply{text: "End."}
	rows = h.waitFor("done", func(r []Row) bool { return len(r) == 1 && r[0].Status == Done })
	if rows[0].Translation != "End." {
		t.Errorf("row = %s", dump(rows))
	}
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d", n)
	}
}

func TestFinishLiveNoiseRemoves(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "euh")
	recv(t, h.tr.calls).reply <- trReply{text: "uh"}
	h.log.FinishLive(id, []byte("W"), "")
	recv(t, h.res.calls).reply <- resReply{err: fmt.Errorf("wrap: %w", stt.ErrNoise)}
	h.waitFor("removed", func(r []Row) bool { return len(r) == 0 })
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d", n)
	}
}

func TestFinishLiveErrorFails(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "Bon")
	recv(t, h.tr.calls).reply <- trReply{text: "Good"}
	h.log.FinishLive(id, []byte("W"), "")
	recv(t, h.res.calls).reply <- resReply{err: errors.New("dial: boom")}
	rows := h.waitFor("failed", func(r []Row) bool { return len(r) == 1 && r[0].Status == Failed })
	if rows[0].Err != "transcription failed: boom" {
		t.Errorf("Err = %q", rows[0].Err)
	}
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d", n)
	}
}

func TestFinishLivePassesLockedLang(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.FinishLive(id, []byte("W"), "en")
	c := recv(t, h.res.calls)
	if c.lang != "en" {
		t.Errorf("lang = %q want en", c.lang)
	}
	if _, ok := c.ctx.Deadline(); !ok {
		t.Error("final pass must have a deadline")
	}
	c.reply <- resReply{err: stt.ErrNoise}
	h.waitFor("removed", func(r []Row) bool { return len(r) == 0 })
}

func TestCancelPendingDropsFinishingLive(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "x")
	recv(t, h.tr.calls).reply <- trReply{text: "X"}
	h.waitFor("translated", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "X" })
	h.log.FinishLive(id, []byte("W"), "")
	c := recv(t, h.res.calls)

	other := h.log.StartLive()
	h.log.UpdateLive(other, "fr", "y")
	lt := recv(t, h.tr.calls)

	h.log.CancelPending()
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("rows = %s", dump(rows))
	}
	if c.ctx.Err() == nil || lt.ctx.Err() == nil {
		t.Error("CancelPending must cancel the final pass and live translations")
	}
	if n := h.log.InFlight(); n != 0 {
		t.Errorf("InFlight = %d", n)
	}
	c.reply <- resolved("fr", "late")
	lt.reply <- trReply{text: "late"}
	h.idle()
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("late results resurrected rows: %s", dump(rows))
	}
	h.log.UpdateLive(other, "fr", "z")
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("UpdateLive after CancelPending: %s", dump(rows))
	}
}

func TestDropLive(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "x")
	c := recv(t, h.tr.calls)
	h.log.DropLive(id)
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("rows = %s", dump(rows))
	}
	if c.ctx.Err() == nil {
		t.Error("DropLive must cancel the live translation")
	}
	c.reply <- trReply{text: "X"}
	h.idle()
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("late live translation applied: %s", dump(rows))
	}
	h.log.UpdateLive(id, "fr", "y")
	h.log.CommitLive(id, "fr", "y")
	h.log.FinishLive(id, []byte("W"), "")
	if rows := h.log.Rows(); len(rows) != 0 {
		t.Fatalf("dropped entry revived: %s", dump(rows))
	}
}

// The lag marker shows as soon as the text moves past its translation, not
// only once a newer live translation arrives.
func TestLiveStaleBeforeNextTranslation(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "a")
	recv(t, h.tr.calls).reply <- trReply{text: "T(a)"}
	h.waitFor("first", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a)" })
	h.log.UpdateLive(id, "fr", "a b")
	c := recv(t, h.tr.calls) // second translation in flight, not answered
	rows := h.log.Rows()
	if len(rows) != 1 || !rows[0].Stale || rows[0].Translation != "T(a)" {
		t.Fatalf("before the second reply: rows = %s stale=%v", dump(rows), rows[0].Stale)
	}
	c.reply <- trReply{text: "T(a b)"}
	rows = h.waitFor("second", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a b)" })
	if rows[0].Stale {
		t.Error("Stale must clear once the current text is translated")
	}
}

func TestLiveLangChangeRetranslates(t *testing.T) {
	h := newHarness(t, 0)
	id := h.log.StartLive()
	h.log.UpdateLive(id, "fr", "a")
	recv(t, h.tr.calls).reply <- trReply{text: "T(a)"}
	h.waitFor("first", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T(a)" })
	h.log.UpdateLive(id, "en", "a")
	if rows := h.log.Rows(); !rows[0].Stale {
		t.Errorf("lang changed: Stale = false, want true")
	}
	c := recv(t, h.tr.calls)
	if c.text != "a" || c.from != "en" || c.to != "fr" {
		t.Fatalf("call = %q %s→%s, want a en→fr", c.text, c.from, c.to)
	}
	c.reply <- trReply{text: "T'(a)"}
	rows := h.waitFor("retranslated", func(r []Row) bool { return len(r) == 1 && r[0].Translation == "T'(a)" })
	if rows[0].Stale {
		t.Error("Stale must clear once retranslated")
	}
}
