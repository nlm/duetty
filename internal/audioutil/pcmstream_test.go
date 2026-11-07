package audioutil

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

// gateSrc hands out one queued chunk per Read, blocking until one is
// pushed, so tests control exactly when audio "arrives".
type gateSrc struct {
	rate   int
	chunks chan []byte // closed = EOF
	err    error       // returned instead of EOF once chunks is closed
	closed atomic.Bool
	done   chan struct{}
}

func newGateSrc(rate int) *gateSrc {
	return &gateSrc{rate: rate, chunks: make(chan []byte, 16), done: make(chan struct{})}
}

func (g *gateSrc) SampleRate() int { return g.rate }
func (g *gateSrc) Read(p []byte) (int, error) {
	select {
	case c, ok := <-g.chunks:
		if !ok {
			if g.err != nil {
				return 0, g.err
			}
			return 0, io.EOF
		}
		return copy(p, c), nil
	case <-g.done:
		return 0, io.ErrClosedPipe
	}
}
func (g *gateSrc) Close() error {
	if g.closed.CompareAndSwap(false, true) {
		close(g.done)
	}
	return nil
}

// ms100 is 100 ms of audio at 24000 Hz: 2400 samples, 4800 bytes.
func ms100(fill byte) []byte { return bytes.Repeat([]byte{fill}, 4800) }

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func waitClosed(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s not closed in time", what)
	}
}

func waitDuration(t *testing.T, e *EagerStream, want time.Duration) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for e.Duration() < want {
		if time.Now().After(deadline) {
			t.Fatalf("Duration() stuck at %v, want %v", e.Duration(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEagerStreamReadyAfterPrebuffer(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 300*time.Millisecond)
	defer e.Close()
	src.chunks <- ms100(1)
	src.chunks <- ms100(2)
	waitDuration(t, e, 200*time.Millisecond)
	if isClosed(e.Ready()) {
		t.Fatal("Ready closed after 200ms of a 300ms prebuffer")
	}
	src.chunks <- ms100(3)
	waitClosed(t, e.Ready(), "Ready")
	if isClosed(e.Received()) {
		t.Fatal("Received closed before src ended")
	}
}

func TestEagerStreamReadyOnShortEOF(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 300*time.Millisecond)
	defer e.Close()
	src.chunks <- ms100(1)
	close(src.chunks)
	waitClosed(t, e.Ready(), "Ready")
	waitClosed(t, e.Received(), "Received")
	if got := e.Duration(); got != 100*time.Millisecond {
		t.Fatalf("Duration = %v, want 100ms", got)
	}
}

func TestEagerStreamReadsEverythingInOrder(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 0)
	defer e.Close()
	var want []byte
	for i := byte(1); i <= 4; i++ {
		c := ms100(i)
		want = append(want, c...)
		src.chunks <- c
	}
	close(src.chunks)
	var got []byte
	buf := make([]byte, 1000)
	for {
		n, err := e.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes, want %d, or order differs", len(got), len(want))
	}
	if n, err := e.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read after EOF = %d, %v", n, err)
	}
}

func TestEagerStreamSrcError(t *testing.T) {
	src := newGateSrc(24000)
	src.err = errBoom
	e := NewEagerStream(src, 0)
	defer e.Close()
	fifty := bytes.Repeat([]byte{7}, 2400) // 50 ms
	src.chunks <- fifty
	close(src.chunks)
	waitClosed(t, e.Received(), "Received")
	if !errors.Is(e.Err(), errBoom) {
		t.Fatalf("Err = %v, want errBoom", e.Err())
	}
	buf := make([]byte, 10000)
	n, err := e.Read(buf)
	if err != nil || n != len(fifty) {
		t.Fatalf("first read = %d, %v; want %d, nil", n, err, len(fifty))
	}
	if _, err := e.Read(buf); !errors.Is(err, errBoom) {
		t.Fatalf("second read err = %v, want errBoom", err)
	}
}

func TestEagerStreamCloseUnblocks(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 0)
	res := make(chan error, 1)
	go func() {
		_, err := e.Read(make([]byte, 100))
		res <- err
	}()
	time.Sleep(20 * time.Millisecond)
	e.Close()
	select {
	case err := <-res:
		if err != io.EOF {
			t.Fatalf("Read after Close = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read still blocked after Close")
	}
	if !src.closed.Load() {
		t.Fatal("src not closed")
	}
}

func TestEagerStreamOddByteChunks(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 0)
	defer e.Close()
	var want []byte
	for i := 0; i < 10; i++ { // 30 bytes in 3-byte chunks
		c := []byte{byte(3 * i), byte(3*i + 1), byte(3*i + 2)}
		want = append(want, c...)
		src.chunks <- c
	}
	close(src.chunks)
	var got []byte
	buf := make([]byte, 7) // odd buffer too
	for {
		n, err := e.Read(buf)
		if n%2 != 0 {
			t.Fatalf("Read returned odd length %d", n)
		}
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEagerStreamUnderruns(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 0)
	defer e.Close()
	go func() {
		time.Sleep(30 * time.Millisecond)
		src.chunks <- ms100(1)
		close(src.chunks)
	}()
	buf := make([]byte, 4800)
	if n, err := e.Read(buf); err != nil || n != 4800 {
		t.Fatalf("read = %d, %v", n, err)
	}
	if got := e.Underruns(); got != 1 {
		t.Fatalf("Underruns = %d, want 1", got)
	}
}

// countingSrc counts Close calls; its Read fails once closed, as an HTTP
// body or a gRPC stream does when aborted.
type countingSrc struct {
	*gateSrc
	closes atomic.Int32
}

func (c *countingSrc) Close() error {
	c.closes.Add(1)
	return c.gateSrc.Close()
}

func TestEagerStreamCloseIsIdempotent(t *testing.T) {
	src := &countingSrc{gateSrc: newGateSrc(24000)}
	e := NewEagerStream(src, 0)
	if err := e.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if got := src.closes.Load(); got != 1 {
		t.Fatalf("src.Close called %d times, want 1", got)
	}
}

// An intentional Close makes src's Read fail (io.ErrClosedPipe here); that
// failure is the close's doing, not a synthesis error worth a warning.
func TestEagerStreamErrIgnoresCloseCausedError(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 0)
	src.chunks <- ms100(1)
	waitDuration(t, e, 100*time.Millisecond)
	e.Close()
	waitClosed(t, e.Received(), "Received")
	if err := e.Err(); err != nil {
		t.Fatalf("Err after Close = %v, want nil", err)
	}
}

func TestEagerStreamStopsAppendingAfterClose(t *testing.T) {
	src := newGateSrc(24000)
	src.done = make(chan struct{}) // Close won't abort Read: chunks keep coming
	e := NewEagerStream(src, 0)
	src.chunks <- ms100(1)
	waitDuration(t, e, 100*time.Millisecond)
	e.mu.Lock()
	e.closed = true // as Close does, minus src.Close, so Read keeps returning data
	e.mu.Unlock()
	src.chunks <- ms100(2)
	close(src.chunks)
	waitClosed(t, e.Received(), "Received")
	if got := e.Duration(); got != 100*time.Millisecond {
		t.Fatalf("Duration = %v after a chunk arrived post-close, want 100ms", got)
	}
}

func TestEagerStreamStartedOnFirstData(t *testing.T) {
	src := newGateSrc(24000)
	e := NewEagerStream(src, 300*time.Millisecond)
	defer e.Close()
	time.Sleep(20 * time.Millisecond)
	if isClosed(e.Started()) {
		t.Fatal("Started closed before any data")
	}
	src.chunks <- bytes.Repeat([]byte{1}, 480) // 10 ms, far under the prebuffer
	waitClosed(t, e.Started(), "Started")
	if isClosed(e.Ready()) {
		t.Fatal("Ready closed with 10ms of a 300ms prebuffer")
	}
}

func TestEagerStreamStartedOnEmptyEnd(t *testing.T) {
	src := newGateSrc(24000)
	src.err = errBoom
	e := NewEagerStream(src, 300*time.Millisecond)
	defer e.Close()
	close(src.chunks)
	waitClosed(t, e.Started(), "Started")
}
