package audioutil

import (
	"errors"
	"io"
	"sync"
	"time"
)

// ErrStreamUnsupported means a would-be streaming backend can't stream after
// all (e.g. a remote server without the RPC). It lives here, not in speech,
// because inferclient (leaf tier) must return it and can't import speech.
var ErrStreamUnsupported = errors.New("streaming synthesis unsupported")

// PCMStream is mono int16 little-endian PCM arriving over time. Read blocks
// until data is available and returns io.EOF at the end; Close aborts the
// producer.
type PCMStream interface {
	SampleRate() int
	Read(p []byte) (int, error)
	Close() error
}

// EagerStream drains a PCMStream in its own goroutine into a growing buffer,
// so a slow consumer (a speaker paced in real time) never backpressures the
// producer (a TTS server generating faster than real time) -- and so the
// consumer can wait for a prebuffer before starting playback, absorbing
// network jitter. The buffer is unbounded on purpose: a stream is one
// sentence at most.
//
// Only whole int16 samples are ever exposed: a producer chunk ending mid
// sample has its odd byte held back until the next chunk completes it, so a
// consumer never plays a half sample.
type EagerStream struct {
	src       PCMStream
	rate      int
	prebuffer time.Duration

	mu        sync.Mutex
	cond      *sync.Cond
	buf       []byte // received, not yet read; always an even length
	total     int    // whole-sample bytes received so far
	err       error  // src's non-EOF error
	received  bool
	closed    bool
	underruns int

	ready       chan struct{}
	readyOnce   sync.Once
	started     chan struct{}
	startedOnce sync.Once
	recvd       chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// NewEagerStream starts reading src immediately. prebuffer is how much audio
// Ready waits for.
func NewEagerStream(src PCMStream, prebuffer time.Duration) *EagerStream {
	e := &EagerStream{
		src:       src,
		rate:      src.SampleRate(),
		prebuffer: prebuffer,
		ready:     make(chan struct{}),
		started:   make(chan struct{}),
		recvd:     make(chan struct{}),
	}
	e.cond = sync.NewCond(&e.mu)
	if prebuffer <= 0 {
		e.readyOnce.Do(func() { close(e.ready) })
	}
	go e.pump()
	return e
}

func (e *EagerStream) pump() {
	chunk := make([]byte, 8192)
	var odd []byte // at most one held-back byte
	for {
		n, err := e.src.Read(chunk)
		if n > 0 {
			data := append(odd, chunk[:n]...)
			odd = nil
			if len(data)%2 != 0 {
				odd = []byte{data[len(data)-1]}
				data = data[:len(data)-1]
			}
			e.mu.Lock()
			full := false
			// Nobody reads after Close; buffering more would only grow
			// memory for a stream already abandoned.
			if !e.closed {
				e.buf = append(e.buf, data...)
				e.total += len(data)
				full = e.rate > 0 && e.durationLocked() >= e.prebuffer
				e.cond.Broadcast()
			}
			e.mu.Unlock()
			if len(data) > 0 {
				e.startedOnce.Do(func() { close(e.started) })
			}
			if full {
				e.readyOnce.Do(func() { close(e.ready) })
			}
		}
		if err != nil {
			e.mu.Lock()
			// A failure after Close is the close's own doing (an aborted
			// HTTP body or gRPC stream), not a synthesis error: reporting it
			// would make every Escape log a spurious warning.
			if err != io.EOF && !e.closed {
				e.err = err
			}
			e.received = true
			e.cond.Broadcast()
			e.mu.Unlock()
			// Short audio never reaches the prebuffer; don't make the
			// consumer wait for audio that will never come.
			e.startedOnce.Do(func() { close(e.started) })
			e.readyOnce.Do(func() { close(e.ready) })
			close(e.recvd)
			return
		}
	}
}

func (e *EagerStream) durationLocked() time.Duration {
	if e.rate <= 0 {
		return 0
	}
	return time.Duration(e.total/2) * time.Second / time.Duration(e.rate)
}

// SampleRate reports the source's sample rate.
func (e *EagerStream) SampleRate() int { return e.rate }

// Read blocks for data. It returns io.EOF once src ended and everything was
// drained (or after Close), or src's own error in place of EOF when src
// failed.
func (e *EagerStream) Read(p []byte) (int, error) {
	if len(p) < 2 {
		return 0, io.ErrShortBuffer
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	counted := false
	for len(e.buf) == 0 {
		if e.closed {
			return 0, io.EOF
		}
		if e.received {
			if e.err != nil {
				return 0, e.err
			}
			return 0, io.EOF
		}
		if !counted {
			e.underruns++
			counted = true
		}
		e.cond.Wait()
	}
	if e.closed {
		return 0, io.EOF
	}
	n := len(p) &^ 1
	if n > len(e.buf) {
		n = len(e.buf)
	}
	copy(p, e.buf[:n])
	e.buf = e.buf[n:]
	return n, nil
}

// Close closes src (once; later calls return the first result) and
// unblocks any reader with io.EOF. Both the speaker and the Synthesizer close
// the same stream on their own exit paths, hence the idempotence.
func (e *EagerStream) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.cond.Broadcast()
		e.mu.Unlock()
		e.closeErr = e.src.Close()
	})
	return e.closeErr
}

// Ready is closed once the prebuffer's worth of audio is buffered, or src
// ended first.
func (e *EagerStream) Ready() <-chan struct{} { return e.ready }

// Started is closed once the first audio arrived, or src ended first (with
// or without any): the time-to-first-chunk a streamed synthesis reports.
func (e *EagerStream) Started() <-chan struct{} { return e.started }

// Received is closed once src returned EOF or an error.
func (e *EagerStream) Received() <-chan struct{} { return e.recvd }

// Err is src's non-EOF error; only meaningful after Received is closed. An
// error src returned after Close is not reported.
func (e *EagerStream) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Duration is the audio received so far (all of it once Received is closed).
func (e *EagerStream) Duration() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.durationLocked()
}

// Underruns counts Reads that found the buffer empty before Received, i.e.
// playback that had to wait on the producer.
func (e *EagerStream) Underruns() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.underruns
}
