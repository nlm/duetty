// Package log is a small structured console logger: the -no-tui fallback
// output path, and previously the only output path before the TUI existed.
package log

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// colorEnabled gates ANSI color codes on the tagged log lines below: on only
// when stdout is a terminal and the user hasn't opted out via NO_COLOR (see
// https://no-color.org), so piping output to a file or the journal doesn't
// fill it with escape codes.
var colorEnabled = os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)

// isTerminal reports whether f is a character device (a terminal), as
// opposed to a regular file or pipe -- the standard dependency-free
// approximation of isatty.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	colorReset   = "\x1b[0m"
	colorDim     = "\x1b[2m"
	colorCyan    = "\x1b[36m"
	colorGreen   = "\x1b[32m"
	colorYellow  = "\x1b[33m"
	colorRed     = "\x1b[31m"
	colorMagenta = "\x1b[35m"
	colorBlue    = "\x1b[34m"
)

// stateEnabled gates the LISTEN/PROC state-transition log lines below on the
// -log-state flag (set once via SetStateLogging before the main loop
// starts). Off by default -- these lines are an opt-in diagnostic aid, not
// something every run should see.
var stateEnabled bool

// SetStateLogging enables or disables the LISTEN/PROC diagnostic lines.
func SetStateLogging(enabled bool) {
	stateEnabled = enabled
}

// line prints one uniformly-shaped log line: a dim timestamp, a fixed-width
// colored tag, then the formatted message, so all output reads as one
// consistent stream.
func line(tag, color, format string, args ...any) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	if colorEnabled {
		fmt.Printf("%s%s%s %s%-6s%s %s\n", colorDim, ts, colorReset, color, tag, colorReset, msg)
		return
	}
	fmt.Printf("%s %-6s %s\n", ts, tag, msg)
}

// Sink receives log events instead of the plain console output below --
// installed by the TUI when it's the active output surface (see SetSink),
// so Info/Warnf calls show up in the chat log
// widget instead of being printed to a terminal the TUI has already taken
// over.
type Sink interface {
	Info(format string, args ...any)
	Warnf(format string, args ...any)
}

// ConsoleSink is the plain-console formatting below (the same tagged-line
// output Info/Warnf fall back to when no Sink is
// installed at all), exposed as a real Sink so it can be composed with
// another one -- e.g. a fan-out sink that writes to both the local terminal
// and a connected remote frontend -- instead of only ever being reachable
// as the implicit "nothing else is installed" behavior. The no-sink
// fallback below delegates to this type rather than duplicating its own
// copy of the formatting, so there's exactly one implementation to keep in
// sync.
type ConsoleSink struct{}

func (ConsoleSink) Info(format string, args ...any) { line("INFO", colorReset, format, args...) }

func (ConsoleSink) Warnf(format string, args ...any) { line("WARN", colorYellow, format, args...) }

// sinkHolder boxes the installed Sink so it can be swapped atomically: a
// mic-capture callback running on its own goroutine (see internal/audioio)
// can call Warnf concurrently with the composition root installing/
// clearing the sink around the TUI's lifetime, so a bare package-level
// `var sink Sink` read/written without synchronization would be a data
// race.
type sinkHolder struct {
	sink Sink
}

var currentSink atomic.Pointer[sinkHolder]

// SetSink installs s as the destination for Info/Warnf
// calls, replacing the plain console output. Pass nil to restore console
// output. Fatalf and the LISTEN/PROC diagnostic lines (Listen/Proc) always
// go to the console regardless -- Fatalf because it's a startup failure
// that happens before/instead of a UI existing, and Listen/Proc because
// they're an opt-in diagnostic aid, not something a chat UI needs to
// render.
func SetSink(s Sink) {
	if s == nil {
		currentSink.Store(nil)
		return
	}
	currentSink.Store(&sinkHolder{sink: s})
}

// sink returns the currently installed Sink, or nil if console output is
// active.
func sink() Sink {
	h := currentSink.Load()
	if h == nil {
		return nil
	}
	return h.sink
}

// Info logs a startup/status line.
func Info(format string, args ...any) {
	if s := sink(); s != nil {
		s.Info(format, args...)
		return
	}
	ConsoleSink{}.Info(format, args...)
}

// Warnf logs a recoverable error: something failed but the program carries
// on (a tool call, a background stream, a best-effort action).
func Warnf(format string, args ...any) {
	if s := sink(); s != nil {
		s.Warnf(format, args...)
		return
	}
	ConsoleSink{}.Warnf(format, args...)
}

// warningsToProcKey marks a ctx whose WarnfContext calls go to Proc.
type warningsToProcKey struct{}

// WithWarningsToProc returns a ctx under which WarnfContext logs via Proc
// instead of Warnf: for work the user doesn't watch -- a background
// task's tool calls -- whose recoverable errors belong in the log, not in
// the chat view (an installed Sink shows every Warnf there).
func WithWarningsToProc(ctx context.Context) context.Context {
	return context.WithValue(ctx, warningsToProcKey{}, true)
}

// WarnfContext is Warnf, or Proc when ctx comes from WithWarningsToProc.
func WarnfContext(ctx context.Context, format string, args ...any) {
	if ctx != nil {
		if quiet, _ := ctx.Value(warningsToProcKey{}).(bool); quiet {
			Proc(format, args...)
			return
		}
	}
	Warnf(format, args...)
}

// Fatalf logs an unrecoverable startup error and exits.
func Fatalf(format string, args ...any) {
	line("FATAL", colorRed, format, args...)
	os.Exit(1)
}

// Listen logs a microphone/VAD state transition -- speech detected or an
// utterance fully captured. No-op unless state logging is enabled.
func Listen(format string, args ...any) {
	if !stateEnabled {
		return
	}
	line("LISTEN", colorMagenta, format, args...)
}

// Proc logs a processing-stage transition (transcription, model
// round-trip, tool-call execution), so a slow or stuck turn is visible
// instead of a silent pause. No-op unless state logging is enabled.
func Proc(format string, args ...any) {
	if !stateEnabled {
		return
	}
	line("PROC", colorBlue, format, args...)
}

// Check logs one preflight checklist line: OK if err is nil, FAIL with the
// error otherwise.
func Check(label string, err error) {
	if err != nil {
		line("FAIL", colorRed, "%s: %v", label, err)
		return
	}
	line("OK", colorGreen, "%s", label)
}
