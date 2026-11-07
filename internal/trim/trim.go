// Package trim cuts leading/trailing non-speech from an utterance before it
// is sent to Whisper; silent padding is a main cause of hallucinations.
package trim

import "time"

const (
	// frameBytes is one Silero frame (32 ms, 512 samples @16k) expressed in
	// 48 kHz s16le mono bytes: 1536 samples * 2.
	frameBytes = 3072
	// bytesPerSecond for 48 kHz s16le mono.
	bytesPerSecond = 48000 * 2
)

// Trim keeps pcm48k (s16le mono, 48 kHz) from margin before the first frame
// with prob >= threshold to one frame plus margin after the last one,
// clamped to the buffer. probs holds one value per 32 ms frame. It returns
// nil when no frame is voiced. The result is sample-aligned.
func Trim(pcm48k []byte, probs []float32, threshold float64, margin time.Duration) []byte {
	first, last := -1, -1
	for i, p := range probs {
		if float64(p) >= threshold {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	m := int(margin.Seconds()*bytesPerSecond) &^ 1
	start := first*frameBytes - m
	if start < 0 {
		start = 0
	}
	end := (last+1)*frameBytes + m
	if end > len(pcm48k) {
		end = len(pcm48k)
	}
	start &^= 1
	end &^= 1
	if end <= start {
		return nil
	}
	return pcm48k[start:end]
}
