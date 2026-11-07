// Package convo is the conversation log: it orders utterances by capture
// time and drives their transcription and translation concurrently.
package convo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nlm/poly/internal/apierr"
	"github.com/nlm/poly/internal/stt"
	"github.com/nlm/poly/internal/translate"
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
}

// Resolver is satisfied by *stt.Resolver.
type Resolver interface {
	Resolve(ctx context.Context, wav16k []byte) (stt.Resolved, error)
	DetectTyped(text string) string
}

type entry struct {
	Row
	cancel context.CancelFunc // cancels the request in flight; nil when idle
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
		e.Status, e.Err = Failed, "transcription failed: "+short(err)
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
	from, to := e.Lang, l.pair[1]
	if from == l.pair[1] {
		to = l.pair[0]
	}
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
		e.Status, e.Err = Failed, "translation failed: "+short(err)
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
// removed (they were never visible), rows translating become Cancelled.
func (l *Log) CancelPending() {
	l.mu.Lock()
	changed := false
	kept := l.rows[:0]
	for _, e := range l.rows {
		switch e.Status {
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

// Rows returns copies of the visible rows (Status != Pending) in Seq order.
func (l *Log) Rows() []Row {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Row, 0, len(l.rows))
	for _, e := range l.rows {
		if e.Status != Pending {
			out = append(out, e.Row)
		}
	}
	return out
}

// InFlight is the number of rows with work outstanding (Pending or Original).
func (l *Log) InFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.rows {
		if e.Status == Pending || e.Status == Original {
			n++
		}
	}
	return n
}

// short renders err as a short user-facing reason with no secrets: the HTTP
// status line, "timeout", or the innermost message of a wrapped error (which
// drops request URLs from net/http errors).
func short(err error) string {
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
