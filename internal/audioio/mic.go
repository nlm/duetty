package audioio

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"

	"github.com/nlm/duetty/internal/log"
)

// dropReportInterval bounds how often accumulated dropped-chunk counts are
// logged (see Mic.Stream's onRecv/reportDrops), so a sustained slow-
// consumer stretch produces one line every few seconds instead of one line
// per dropped chunk.
const dropReportInterval = 2 * time.Second

// chunkBytes is the size of each PCM chunk delivered on Mic.Stream's
// channel: 4096 bytes (2048 mono s16 samples, ~43ms at 48kHz), matching the
// chunk size the original robot daemon's mic stream used, so downstream VAD
// timing tuned against that cadence needs no changes.
const chunkBytes = 4096

// MicChunkBufferSize bounds Mic.Stream's own delivery channel below, and is
// exported specifically so any other producer feeding the same "raw PCM
// chunks" shape (e.g. internal/app's remoteFrontend, forwarding a connected
// frontend's AudioChunk events instead of a local device) can size its own
// channel identically by reference rather than a second hardcoded literal
// that silently drifts out of sync if this one is ever retuned.
const MicChunkBufferSize = 64

// Mic is a MicSource backed by a local capture device via malgo.
type Mic struct {
	device string // device name/index to select, or "" for the system default
}

// NewMic returns a Mic that captures from the named device, or the system
// default capture device if device is "".
func NewMic(device string) *Mic {
	return &Mic{device: device}
}

// Stream implements MicSource.
func (m *Mic) Stream(ctx context.Context) (<-chan []byte, error) {
	actx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(msg string) {
		// miniaudio's log messages already end in their own line terminator
		// (\n, or \r\n on some backends) -- the log's line() unconditionally
		// appends another "\n", so passing msg through as-is doubles up into
		// a blank line after every entry. Trim it, and drop now-empty
		// messages (miniaudio also emits blank-line-only log calls) instead
		// of printing an empty "WARN audio: " line.
		msg = strings.TrimRight(msg, "\r\n")
		if msg == "" {
			return
		}
		// Info, not Warnf: miniaudio logs every backend probe and symbol
		// load here (debug chatter, not warnings). A real failure comes back
		// as Stream's error.
		log.Info("audio: %s", msg)
	})
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}

	deviceID, err := findDevice(actx.Context, malgo.Capture, m.device)
	if err != nil {
		_ = actx.Uninit()
		actx.Free()
		return nil, err
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = 1
	deviceConfig.SampleRate = SampleRate
	if deviceID != nil {
		deviceConfig.Capture.DeviceID = deviceID.Pointer()
	}

	out := make(chan []byte, MicChunkBufferSize)
	var pending []byte
	var dropped atomic.Uint64

	// onRecv runs synchronously on miniaudio's own real-time audio thread
	// (not a goroutine malgo spawns per call), so it must never block --
	// logging here directly (as opposed to bumping this lock-free counter)
	// risked stalling the audio thread on log.Sink's TUI dispatch, which
	// itself waits on tview's event loop; a stalled event loop -- e.g.
	// during the brief window before Application.Run() starts draining it,
	// or after it returns -- would then hang audio capture indefinitely.
	onRecv := func(_, in []byte, _ uint32) {
		pending = append(pending, in...)
		for len(pending) >= chunkBytes {
			chunk := make([]byte, chunkBytes)
			copy(chunk, pending[:chunkBytes])
			pending = pending[chunkBytes:]
			select {
			case out <- chunk:
			default:
				dropped.Add(1)
			}
		}
	}

	device, err := malgo.InitDevice(actx.Context, deviceConfig, malgo.DeviceCallbacks{Data: onRecv})
	if err != nil {
		_ = actx.Uninit()
		actx.Free()
		return nil, fmt.Errorf("init capture device: %w", err)
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		_ = actx.Uninit()
		actx.Free()
		return nil, fmt.Errorf("start capture device: %w", err)
	}

	go func() {
		<-ctx.Done()
		device.Uninit()
		_ = actx.Uninit()
		actx.Free()
		close(out)
	}()

	go reportDrops(ctx, &dropped)

	return out, nil
}

// reportDrops periodically logs how many audio chunks onRecv has dropped
// since the last report, from an ordinary goroutine rather than the audio
// callback thread itself (see onRecv's doc comment). Silent when nothing
// was dropped in a given interval.
func reportDrops(ctx context.Context, dropped *atomic.Uint64) {
	ticker := time.NewTicker(dropReportInterval)
	defer ticker.Stop()
	var lastReported uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			total := dropped.Load()
			if n := total - lastReported; n > 0 {
				log.Warnf("mic: dropped %d audio chunk(s) in the last %s, consumer too slow", n, dropReportInterval)
				lastReported = total
			}
		}
	}
}
