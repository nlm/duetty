// Package stderrguard offers one process-wide way to temporarily silence
// the real stderr file descriptor (fd 2) around a native call that writes
// to it directly, bypassing Go's os.Stderr. Two unrelated native library
// families in this codebase need this for two different messages --
// sherpa-onnx-go's C++ core prints "Creating a resampler: ..." via a raw
// libc fprintf(stderr, ...) (see internal/sherpazipformer/
// internal/sherpawhisper/internal/sherpatts), and onnxruntime's CoreML
// execution provider makes Apple's own CoreML/ANE runtime print "Context
// leak detected, CoreAnalytics returned false" the same way (see
// internal/onnxenv.NewSessionOptions and every internal/silerovad/
// internal/facerec/internal/voicerec/internal/textembed inference call) --
// but fd 2 is one single, process-wide OS resource, so every caller
// silencing it must serialize through the same lock rather than each
// package independently juggling its own copy of the dup/dup2/restore
// dance. An earlier version had each package duplicate this helper
// (matching this codebase's usual small-helper-duplication convention,
// see CLAUDE.md's "Tool schemas can drift across packages" paragraph) --
// a review caught that this was unsafe, not just repetitive: a detached
// goroutine (e.g. internal/conversation.HandleTurn's voice-embedding
// computation, which can keep running past its turn's own cancellation)
// calling internal/voicerec's Embed could still be mid-redirect when
// internal/silerovad's VAD.Feed (resumed the instant the mic unmutes)
// started its own redirect concurrently -- two interleaved dup/dup2/
// restore sequences on the same fd can leave the wrong saved fd restored,
// permanently losing the process's real stderr (including future Go
// panic traces), which is strictly worse than either cosmetic message
// this package exists to silence. Suppress's package-level mutex makes
// that structurally impossible: only one redirect is ever in flight
// process-wide, and every other caller (from any package) simply blocks
// for the (sub-millisecond, single-inference-call) duration of the
// current one rather than racing it.
package stderrguard

import (
	"os"
	"sync"
	"syscall"
)

var mu sync.Mutex

// Suppress runs fn with the process's real stderr file descriptor (fd 2)
// temporarily redirected to /dev/null, restoring it unconditionally
// afterward. Serialized process-wide via a package-level mutex -- see the
// package doc comment for why that's required, not optional. Scoped
// narrowly to just fn's duration (not, say, a whole session's lifetime)
// so a genuine Go panic elsewhere still gets its stack trace on stderr,
// and so concurrent callers only block briefly rather than for an
// unbounded time.
func Suppress(fn func()) {
	mu.Lock()
	defer mu.Unlock()

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		fn()
		return
	}
	defer devNull.Close()

	saved, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		fn()
		return
	}
	defer syscall.Close(saved)

	if err := syscall.Dup2(int(devNull.Fd()), int(os.Stderr.Fd())); err != nil {
		fn()
		return
	}
	defer syscall.Dup2(saved, int(os.Stderr.Fd()))

	fn()
}
