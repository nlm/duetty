// Package audioio provides local microphone capture via malgo (miniaudio).
package audioio

import "context"

// SampleRate is the PCM sample rate (Hz) produced by MicSource.Stream --
// 48000Hz mono 16-bit signed little-endian, matching the format the rest of
// the pipeline (VAD, Whisper upload, voice-embedding) already expects.
const SampleRate = 48000

// MicSource streams raw microphone PCM.
type MicSource interface {
	// Stream starts capture and returns a channel of raw PCM chunks
	// (48000Hz mono s16le, see SampleRate), closed when ctx is done or
	// capture fails.
	Stream(ctx context.Context) (<-chan []byte, error)
}
