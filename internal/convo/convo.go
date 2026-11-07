// Package convo is the conversation log: it orders utterances by capture
// time and drives their transcription and translation concurrently.
package convo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nlm/duetty/internal/apierr"
	"github.com/nlm/duetty/internal/stt"
	"github.com/nlm/duetty/internal/translate"
)

// RequestTimeout bounds every transcription and translation request.
const RequestTimeout = 30 * time.Second

// Status is a row's stage.
type Status int

const (
	Pending   Status = iota // transcribing; hidden
	Original                // original known, translation in flight
	Done                    // translated
	Cancelled               // cancelled by the user while translating
	Failed                  // transcription or translation failed; see Err
	Live                    // provisional text of the turn being spoken; see StartLive
)

func (s Status) String() string {
	switch s {
	case Pending:
		return "pending"
	case Original:
		return "original"
	case Done:
		return "done"
	case Cancelled:
		return "cancelled"
	case Failed:
		return "failed"
	case Live:
		return "live"
	}
	return "unknown"
}

// Row is one utterance.
type Row struct {
	Seq                   uint64    // capture order
	At                    time.Time // capture time
	Status                Status
	Lang                  string // source language
	Original, Translation string
	Err                   string // short, user-facing, no secrets
	Stale                 bool   // Live only: Translation was made for older text or another language
}

// Resolver is satisfied by *stt.Resolver.
type Resolver interface {
	Resolve(ctx context.Context, wav16k []byte) (stt.Resolved, error)
	ResolveLocked(ctx context.Context, wav16k []byte, lang string) (stt.Resolved, error)
	DetectTyped(text string) string
}

type entry struct {
	Row
	cancel context.CancelFunc // cancels the request in flight; nil when idle

	// Live entries only.
	liveID    LiveID
	finishing bool   // FinishLive's final pass is running
	liveBusy  bool   // a live translation is in flight
	liveGen   uint64 // bumped by CommitLive: older live results are void
	sentLang  string // language and text last sent for live translation
	sentText  string
	transLang string // language and text the current Translation was made for
	transText string
	liveStop  context.CancelFunc // cancels the live translation in flight
}

// Log is the ordered conversation. All methods are safe for concurrent use.
type Log struct {
	pair     [2]string
	r        Resolver
	tr       translate.Translator
	contextN int
	onChange func()
	base     context.Context
	timeout  time.Duration
	wg       sync.WaitGroup // worker goroutines (tests wait on it)

	mu      sync.Mutex
	nextSeq uint64
	rows    []*entry // sorted by Seq (appended at submit time)
	nextLiv LiveID
	live    map[LiveID]*entry // Live entries not yet resolved or dropped
}

// New builds a Log for a fixed language pair. onChange (may be nil) is
// called, without any Log lock held, whenever the visible rows or the
// in-flight count change; it may call Rows and InFlight.
//
// onChange may be called concurrently from multiple goroutines (request
// workers, and whichever goroutine calls SubmitText or CancelPending), so it
// must be cheap, non-blocking and safe for concurrent use. Two concurrent
// calls may observe snapshots in either order: a caller that publishes the
// rows must serialise its snapshot-and-publish step.
func New(pair [2]string, r Resolver, tr translate.Translator, contextN int, onChange func()) *Log {
	if onChange == nil {
		onChange = func() {}
	}
	return &Log{
		pair: pair, r: r, tr: tr, contextN: max(contextN, 0), onChange: onChange,
		base: context.Background(), timeout: RequestTimeout,
		live: map[LiveID]*entry{},
	}
}

// add appends a new entry with the next Seq. Caller holds l.mu.
func (l *Log) add(e *entry) {
	l.nextSeq++
	e.Seq = l.nextSeq
	e.At = time.Now()
	l.rows = append(l.rows, e)
}

// SubmitAudio queues an utterance for transcription; its Seq is assigned now.
func (l *Log) SubmitAudio(wav16k []byte) {
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	e := &entry{cancel: cancel}
	l.mu.Lock()
	l.add(e)
	l.mu.Unlock()
	l.onChange() // in-flight count changed

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer cancel()
		res, err := l.r.Resolve(ctx, wav16k)
		l.transcribed(e, res, err)
	}()
}

// transcribed applies a Resolve result unless the row was cancelled.
func (l *Log) transcribed(e *entry, res stt.Resolved, err error) {
	l.mu.Lock()
	if e.Status != Pending { // cancelled (and removed) meanwhile
		l.mu.Unlock()
		return
	}
	e.cancel = nil
	switch {
	case errors.Is(err, stt.ErrNoise):
		l.remove(e)
		l.mu.Unlock()
	case err != nil:
		e.Status, e.Err = Failed, "transcription failed: "+ShortError(err)
		l.mu.Unlock()
	default:
		e.Status, e.Lang, e.Original = Original, res.Lang, res.Text
		l.startTranslation(e) // unlocks
	}
	l.onChange()
}

// SubmitText adds a typed utterance; blank text is ignored. The row is
// visible at once (no transcription stage) and then translated.
func (l *Log) SubmitText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	lang := l.r.DetectTyped(text)
	l.mu.Lock()
	e := &entry{Row: Row{Status: Original, Lang: lang, Original: text}}
	l.add(e)
	l.startTranslation(e) // unlocks
	l.onChange()
}

// startTranslation launches translation of e (Status Original). Caller
// holds l.mu; it is released before returning.
func (l *Log) startTranslation(e *entry) {
	from, to := l.direction(e.Lang)
	history := l.history(e.Seq)
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	e.cancel = cancel
	text := e.Original
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		defer cancel()
		out, err := l.tr.Translate(ctx, text, from, to, history)
		l.translated(e, out, err)
	}()
}

// direction returns the translation direction for source language lang.
func (l *Log) direction(lang string) (from, to string) {
	if lang == l.pair[1] {
		return lang, l.pair[0]
	}
	return lang, l.pair[1]
}

// history returns the last contextN Done rows before seq, oldest first.
// Caller holds l.mu.
func (l *Log) history(seq uint64) []translate.Pair {
	var h []translate.Pair
	for i := len(l.rows) - 1; i >= 0 && len(h) < l.contextN; i-- {
		e := l.rows[i]
		if e.Seq < seq && e.Status == Done {
			h = append(h, translate.Pair{Lang: e.Lang, Original: e.Original, Translation: e.Translation})
		}
	}
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return h
}

// translated applies a Translate result unless the row was cancelled.
func (l *Log) translated(e *entry, out string, err error) {
	l.mu.Lock()
	if e.Status != Original {
		l.mu.Unlock()
		return
	}
	e.cancel = nil
	if err != nil {
		e.Status, e.Err = Failed, "translation failed: "+ShortError(err)
	} else {
		e.Status, e.Translation = Done, out
	}
	l.mu.Unlock()
	l.onChange()
}

// remove drops e from the log and marks it so late results are ignored.
// Caller holds l.mu.
func (l *Log) remove(e *entry) {
	e.Status = Cancelled
	for i, x := range l.rows {
		if x == e {
			l.rows = append(l.rows[:i], l.rows[i+1:]...)
			return
		}
	}
}

// CancelPending cancels all outstanding work: rows still transcribing are
// removed (they were never visible), rows translating become Cancelled, and
// every Live entry (finishing or not) is dropped.
func (l *Log) CancelPending() {
	l.mu.Lock()
	changed := false
	kept := l.rows[:0]
	for _, e := range l.rows {
		switch e.Status {
		case Live:
			l.dropLive(e)
			changed = true
			continue
		case Pending, Original:
			if e.cancel != nil {
				e.cancel()
				e.cancel = nil
			}
			changed = true
			if e.Status == Pending {
				e.Status = Cancelled // marks the dropped entry for late results
				continue
			}
			e.Status = Cancelled
		}
		kept = append(kept, e)
	}
	clear(l.rows[len(kept):])
	l.rows = kept
	l.mu.Unlock()
	if changed {
		l.onChange()
	}
}

// Rows returns copies of the visible rows in Seq order: all but Pending
// rows and Live entries with no text yet.
func (l *Log) Rows() []Row {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Row, 0, len(l.rows))
	for _, e := range l.rows {
		if e.Status != Pending && (e.Status != Live || e.Original != "") {
			out = append(out, e.Row)
		}
	}
	return out
}

// InFlight is the number of rows with work outstanding: Pending, Original,
// and Live entries whose final pass (FinishLive) is running.
func (l *Log) InFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.rows {
		if e.Status == Pending || e.Status == Original || (e.Status == Live && e.finishing) {
			n++
		}
	}
	return n
}

// LiveID identifies a Live entry.
type LiveID uint64

// StartLive adds a Live entry for the turn being spoken; its Seq is assigned
// now. It stays hidden from Rows while its Original is empty.
func (l *Log) StartLive() LiveID {
	l.mu.Lock()
	l.nextLiv++
	id := l.nextLiv
	e := &entry{Row: Row{Status: Live}, liveID: id}
	l.add(e)
	l.live[id] = e
	l.mu.Unlock()
	return id
}

// activeLive returns the Live entry id unless it is gone or finishing.
// Caller holds l.mu.
func (l *Log) activeLive(id LiveID) *entry {
	e := l.live[id]
	if e == nil || e.finishing {
		return nil
	}
	return e
}

// UpdateLive sets the provisional Lang and Original of a Live entry and
// translates the new text (latest wins). Ignored once finishing or gone.
func (l *Log) UpdateLive(id LiveID, lang, text string) {
	l.mu.Lock()
	e := l.activeLive(id)
	if e == nil {
		l.mu.Unlock()
		return
	}
	e.Lang, e.Original = lang, text
	e.Stale = liveStale(e)
	l.maybeTranslateLive(e)
	l.mu.Unlock()
	l.onChange()
}

// liveStale reports whether e's Translation was made for other text or
// another language than its current Original. Caller holds l.mu.
func liveStale(e *entry) bool {
	return e.Translation != "" && (e.transText != e.Original || e.transLang != e.Lang)
}

// maybeTranslateLive starts a live translation of e's current text unless
// one is in flight (its completion picks up the latest text) or the same
// text and language were already sent. Caller holds l.mu (kept held).
func (l *Log) maybeTranslateLive(e *entry) {
	if e.liveBusy || e.Original == "" || (e.Original == e.sentText && e.Lang == e.sentLang) {
		return
	}
	lang, text, gen := e.Lang, e.Original, e.liveGen
	from, to := l.direction(lang)
	history := l.history(e.Seq)
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	e.liveBusy, e.sentLang, e.sentText, e.liveStop = true, lang, text, cancel
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer cancel()
		out, err := l.tr.Translate(ctx, text, from, to, history)
		l.liveTranslated(e, gen, lang, text, out, err)
	}()
}

// liveTranslated applies a live translation result while e is still Live,
// not finishing and not committed since, then sends any newer text.
func (l *Log) liveTranslated(e *entry, gen uint64, lang, text, out string, err error) {
	l.mu.Lock()
	e.liveBusy, e.liveStop = false, nil
	if e.Status != Live || e.finishing {
		l.mu.Unlock()
		return
	}
	if err == nil && gen == e.liveGen {
		e.Translation, e.transLang, e.transText = out, lang, text
		e.Stale = liveStale(e)
	}
	l.maybeTranslateLive(e)
	l.mu.Unlock()
	l.onChange()
}

// CommitLive turns a finished sentence of a Live entry into a normal row:
// the row takes the entry's place and Seq and is translated as usual; the
// Live entry gets the next Seq, moves to the end and starts over empty.
func (l *Log) CommitLive(id LiveID, lang, text string) {
	l.mu.Lock()
	e := l.activeLive(id)
	if e == nil {
		l.mu.Unlock()
		return
	}
	n := &entry{Row: Row{Seq: e.Seq, At: e.At, Status: Original, Lang: lang, Original: text}}
	i := slices.Index(l.rows, e)
	l.rows = slices.Delete(l.rows, i, i+1)
	l.rows = slices.Insert(l.rows, i, n) // n keeps e's Seq, so it goes where e was
	l.add(e)
	e.Original, e.Translation, e.Stale = "", "", false
	e.sentLang, e.sentText, e.transLang, e.transText = "", "", "", ""
	e.liveGen++
	l.startTranslation(n) // unlocks
	l.onChange()
}

// FinishLive runs the final pass of a Live entry (ResolveLocked with lang,
// "" for a full resolve). Meanwhile the entry keeps its provisional text and
// counts in InFlight. Then it becomes a normal row with the final text, is
// removed if the pass found noise, or becomes Failed.
func (l *Log) FinishLive(id LiveID, wav16k []byte, lang string) {
	l.mu.Lock()
	e := l.activeLive(id)
	if e == nil {
		l.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	e.finishing, e.cancel = true, cancel
	l.wg.Add(1)
	l.mu.Unlock()
	l.onChange() // in-flight count changed

	go func() {
		defer l.wg.Done()
		defer cancel()
		res, err := l.r.ResolveLocked(ctx, wav16k, lang)
		l.finished(e, res, err)
	}()
}

// finished applies a final pass result unless the entry was dropped.
func (l *Log) finished(e *entry, res stt.Resolved, err error) {
	l.mu.Lock()
	if e.Status != Live { // dropped meanwhile
		l.mu.Unlock()
		return
	}
	delete(l.live, e.liveID)
	e.cancel = nil // a live translation still in flight is discarded on return
	e.Translation, e.Stale = "", false
	switch {
	case errors.Is(err, stt.ErrNoise):
		l.remove(e)
		l.mu.Unlock()
	case err != nil:
		e.Status, e.Original, e.Err = Failed, "", "transcription failed: "+ShortError(err)
		l.mu.Unlock()
	default:
		e.Status, e.Lang, e.Original = Original, res.Lang, res.Text
		l.startTranslation(e) // unlocks
	}
	l.onChange()
}

// DropLive removes a Live entry and cancels its requests.
func (l *Log) DropLive(id LiveID) {
	l.mu.Lock()
	e := l.live[id]
	if e == nil {
		l.mu.Unlock()
		return
	}
	l.dropLive(e)
	l.remove(e)
	l.mu.Unlock()
	l.onChange()
}

// dropLive cancels a Live entry's requests, forgets it and marks it so late
// results are ignored. It does not touch l.rows. Caller holds l.mu.
func (l *Log) dropLive(e *entry) {
	for _, c := range []context.CancelFunc{e.cancel, e.liveStop} {
		if c != nil {
			c()
		}
	}
	e.cancel, e.liveStop = nil, nil
	e.Status = Cancelled
	delete(l.live, e.liveID)
}

// ShortError renders err as a short user-facing reason with no secrets: the HTTP
// status line, "timeout", or the innermost message of a wrapped error (which
// drops request URLs from net/http errors).
func ShortError(err error) string {
	var se *apierr.StatusError
	switch {
	case errors.As(err, &se):
		return se.Status
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}
