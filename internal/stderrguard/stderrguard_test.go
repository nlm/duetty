package stderrguard

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestSuppressSilencesOnlyDuringFn proves Suppress actually redirects the
// real fd behind os.Stderr for exactly the duration of fn, and
// unconditionally restores it afterward -- writing directly to that raw fd
// number (not through the os.Stderr Go value), mirroring how the native
// libraries this package exists for bypass os.Stderr entirely.
func TestSuppressSilencesOnlyDuringFn(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fd := int(os.Stderr.Fd())
	write := func(s string) {
		if _, err := syscall.Write(fd, []byte(s)); err != nil {
			t.Fatalf("syscall.Write: %v", err)
		}
	}

	write("before\n")
	Suppress(func() {
		write("during\n")
	})
	write("after\n")

	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "before") {
		t.Error("output is missing the pre-suppression write")
	}
	if strings.Contains(got, "during") {
		t.Errorf("output = %q, want the write made during Suppress's fn to be silenced", got)
	}
	if !strings.Contains(got, "after") {
		t.Error("output is missing the post-suppression write -- stderr was not correctly restored")
	}
}

// TestSuppressSerializesConcurrentCallers is the regression test for the
// exact bug a review caught: two goroutines calling Suppress concurrently
// (standing in for, e.g., a detached voicerec.Embed goroutine and a
// concurrently-running silerovad.VAD.Feed) must never have their fn calls
// running at the same time. Detected with a shared counter that must never
// exceed 1 while inside fn, checked under -race too so a real data race in
// the fd-juggling itself (not just a logical overlap) would also be
// caught.
func TestSuppressSerializesConcurrentCallers(t *testing.T) {
	var active int32
	var maxActive int32
	var wg sync.WaitGroup

	const goroutines = 8
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			Suppress(func() {
				n := atomic.AddInt32(&active, 1)
				for {
					m := atomic.LoadInt32(&maxActive)
					if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxActive); got > 1 {
		t.Errorf("max concurrently-active Suppress callers = %d, want at most 1 -- Suppress is not correctly serializing", got)
	}
}
