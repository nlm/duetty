package log

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}

type fakeSink struct {
	infos, warns []string
}

func (f *fakeSink) Info(format string, args ...any)  { f.infos = append(f.infos, format) }
func (f *fakeSink) Warnf(format string, args ...any) { f.warns = append(f.warns, format) }

func TestSetSinkDispatchesInsteadOfConsole(t *testing.T) {
	sink := &fakeSink{}
	SetSink(sink)
	defer SetSink(nil)

	out := captureStdout(t, func() {
		Info("info %d", 1)
		Warnf("warn %d", 2)
	})

	if out != "" {
		t.Errorf("console output = %q, want empty while a sink is installed", out)
	}
	if len(sink.infos) != 1 {
		t.Errorf("sink.infos = %v, want 1 entry", sink.infos)
	}
	if len(sink.warns) != 1 {
		t.Errorf("sink.warns = %v, want 1 entry", sink.warns)
	}
}

func TestSetSinkNilRestoresConsoleOutput(t *testing.T) {
	SetSink(&fakeSink{})
	SetSink(nil)

	out := captureStdout(t, func() {
		Info("back to console")
	})
	if !strings.Contains(out, "back to console") {
		t.Errorf("console output = %q, want it to contain the logged text after SetSink(nil)", out)
	}
	if !strings.Contains(out, "INFO") {
		t.Errorf("console output = %q, want the INFO tag", out)
	}
}

func TestListenProcNoopUnlessStateLoggingEnabled(t *testing.T) {
	SetStateLogging(false)
	defer SetStateLogging(false)

	out := captureStdout(t, func() {
		Listen("speech detected")
		Proc("thinking")
	})
	if out != "" {
		t.Errorf("Listen/Proc output = %q, want empty when state logging is disabled", out)
	}

	SetStateLogging(true)
	out = captureStdout(t, func() {
		Listen("speech detected")
	})
	if !strings.Contains(out, "LISTEN") || !strings.Contains(out, "speech detected") {
		t.Errorf("Listen output = %q, want it to contain LISTEN and the message", out)
	}

	out = captureStdout(t, func() {
		Proc("thinking")
	})
	if !strings.Contains(out, "PROC") || !strings.Contains(out, "thinking") {
		t.Errorf("Proc output = %q, want it to contain PROC and the message", out)
	}
}

func TestCheckOKAndFail(t *testing.T) {
	out := captureStdout(t, func() {
		Check("piper binary", nil)
	})
	if !strings.Contains(out, "OK") || !strings.Contains(out, "piper binary") {
		t.Errorf("Check(nil) output = %q, want OK and the label", out)
	}

	out = captureStdout(t, func() {
		Check("ffmpeg binary", errors.New("not found"))
	})
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "ffmpeg binary") || !strings.Contains(out, "not found") {
		t.Errorf("Check(err) output = %q, want FAIL, the label, and the error", out)
	}
}

// TestConsoleSinkMatchesNoSinkFallback is the regression test for the
// ConsoleSink extraction: calling ConsoleSink{}'s methods directly must
// produce the same tag and message content as the package-level
// Info/Warnf functions do when no Sink is installed --
// proving the no-sink fallback now genuinely delegates to ConsoleSink
// rather than keeping its own separate copy of the formatting that could
// drift out of sync. Compares by substring, not full-line equality, since
// two independently-timestamped lines could legitimately differ by their
// "HH:MM:SS" prefix alone if a test happens to straddle a second boundary.
func TestConsoleSinkMatchesNoSinkFallback(t *testing.T) {
	defer SetSink(nil)

	cases := []struct {
		name string
		fn   func()
		want []string
	}{
		{"Info", func() { ConsoleSink{}.Info("info %d", 1) }, []string{"INFO", "info 1"}},
		{"Warnf", func() { ConsoleSink{}.Warnf("warn %d", 2) }, []string{"WARN", "warn 2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, tc.fn)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("ConsoleSink{}.%s output = %q, want it to contain %q", tc.name, out, want)
				}
			}
		})
	}
}

// TestConsoleSinkImplementsSink is a compile-time-flavored check that
// ConsoleSink satisfies Sink -- kept as a runtime test (rather than a bare
// `var _ Sink = ConsoleSink{}` package-level assertion) so it shows up in
// test output like every other check in this file.
func TestConsoleSinkImplementsSink(t *testing.T) {
	var s Sink = ConsoleSink{}
	if s == nil {
		t.Fatal("ConsoleSink{} must not satisfy Sink as a nil value")
	}
}

// TestConcurrentSetSinkAndLoggingIsRaceFree exercises SetSink being
// installed/cleared concurrently with logging calls from another goroutine
// -- mirrors the real shape of a startup race (a mic-capture callback
// goroutine warning about a dropped chunk while the composition root
// installs the TUI's sink) that a bare, unsynchronized package-level `var
// sink Sink` would trip under `go test -race`.
func TestConcurrentSetSinkAndLoggingIsRaceFree(t *testing.T) {
	defer SetSink(nil)
	sink := &fakeSink{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				SetSink(sink)
			} else {
				SetSink(nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		captureStdout(t, func() {
			for i := 0; i < 200; i++ {
				Warnf("warning %d", i)
			}
		})
	}()
	wg.Wait()
}
