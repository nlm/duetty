// Detector runs the Smart Turn v3.2 ONNX model: given the audio of the
// user's turn so far, it returns the probability that the user has finished
// speaking. internal/vad's fixed-silence endpointing then only needs to wait
// out a short pause before asking, instead of a long worst-case silence.
//
// Model facts: pipecat-ai/smart-turn-v3 v3.2 (BSD-2), the 8.7 MB int8 CPU
// export. Input "input_features" float32 [batch,80,800] (the Whisper log-mel
// of the last 8 s of 16 kHz audio, see Features), output "logits" float32
// [batch,1] -- despite the name, the graph ends in a sigmoid, so the value is
// already a probability in [0,1]. Vendor-reported accuracy: French 96 %,
// English 94.3 %.
//
// This package is a leaf on purpose: internal/vad defines its own small
// TurnJudge interface rather than importing it, and the composition root
// wires the two, so the VAD state machine stays testable without ONNX.
// Like the other onnxruntime_go-backed packages it only touches ONNX once New
// is called.
package smartturn

import (
	"errors"
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/nlm/poly/internal/audioutil"
	"github.com/nlm/poly/internal/onnxenv"
	"github.com/nlm/poly/internal/stderrguard"
)

const (
	inputName  = "input_features"
	outputName = "logits"
)

var errClosed = errors.New("smart-turn detector closed")

// Detector is safe for concurrent use. mu serializes Predict against Close,
// not just Predict against itself: the composition root never joins the VAD
// listener's goroutine at shutdown, so a pause held across Ctrl+C can call
// Predict after (or while) Close destroys the shared session. Under mu,
// Predict sees either the live session or nil -- never a destroyed one.
type Detector struct {
	mu      sync.Mutex
	session *ort.DynamicAdvancedSession
}

// New loads the model and runs one warm-up Predict on 1 s of silence, so the
// first real turn doesn't pay onnxruntime's lazy-initialisation cost on the
// latency-critical path. threads <= 0 leaves onnxruntime's default.
func New(ortLibPath, modelPath, provider string, threads int) (*Detector, error) {
	if err := onnxenv.Ensure(ortLibPath); err != nil {
		return nil, fmt.Errorf("initialize onnxruntime environment: %w", err)
	}
	ins, outs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("read smart-turn model %q metadata: %w", modelPath, err)
	}
	if !hasName(ins, inputName) {
		return nil, fmt.Errorf("smart-turn model %q has no input %q (inputs: %v) -- not a Smart Turn v3 model?", modelPath, inputName, names(ins))
	}
	if !hasName(outs, outputName) {
		return nil, fmt.Errorf("smart-turn model %q has no output %q (outputs: %v) -- not a Smart Turn v3 model?", modelPath, outputName, names(outs))
	}

	opts, err := onnxenv.NewSessionOptions(provider)
	if err != nil {
		return nil, fmt.Errorf("create smart-turn session options: %w", err)
	}
	defer opts.Destroy()
	if threads > 0 {
		if err := opts.SetIntraOpNumThreads(threads); err != nil {
			return nil, fmt.Errorf("set smart-turn intra-op thread count: %w", err)
		}
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath, []string{inputName}, []string{outputName}, opts)
	if err != nil {
		return nil, fmt.Errorf("load smart-turn model %q: %w", modelPath, err)
	}
	d := &Detector{session: session}
	if _, err := d.Predict(make([]byte, 2*48000)); err != nil {
		d.Close()
		return nil, fmt.Errorf("smart-turn warm-up: %w", err)
	}
	return d, nil
}

func hasName(infos []ort.InputOutputInfo, name string) bool {
	for _, i := range infos {
		if i.Name == name {
			return true
		}
	}
	return false
}

func names(infos []ort.InputOutputInfo) []string {
	out := make([]string, len(infos))
	for i, v := range infos {
		out[i] = v.Name
	}
	return out
}

// Predict takes raw mono s16le PCM at 48 kHz (the app-wide mic contract),
// resamples to 16 kHz at this package's own edge, and returns the
// probability the turn is complete. After Close it returns an error instead
// of touching the released session; internal/vad treats any judge error as
// "finished" (fails open), so a late call at shutdown just ends the turn.
func (d *Detector) Predict(pcm48k []byte) (float32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return 0, errClosed
	}
	n := len(pcm48k) / 2
	samples := make([]float64, n)
	for i := 0; i < n; i++ {
		samples[i] = float64(int16(uint16(pcm48k[2*i])|uint16(pcm48k[2*i+1])<<8)) / 32768
	}
	rs, err := audioutil.ResampleTo16kHz(samples, audioutil.NativeSampleRate)
	if err != nil {
		return 0, fmt.Errorf("resample for smart-turn: %w", err)
	}
	s16 := make([]float32, len(rs))
	for i, v := range rs {
		s16[i] = float32(v)
	}
	feats := Features(s16)

	in, err := ort.NewTensor(ort.NewShape(1, NMels, NFrames), feats)
	if err != nil {
		return 0, fmt.Errorf("build smart-turn input tensor: %w", err)
	}
	defer in.Destroy()

	outputs := make([]ort.Value, 1)
	var runErr error
	stderrguard.Suppress(func() {
		runErr = d.session.Run([]ort.Value{in}, outputs)
	})
	if runErr != nil {
		return 0, fmt.Errorf("run smart-turn model: %w", runErr)
	}
	defer func() {
		for _, o := range outputs {
			if o != nil {
				o.Destroy()
			}
		}
	}()
	out, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return 0, fmt.Errorf("smart-turn output has unexpected type %T (want float32 tensor)", outputs[0])
	}
	data := out.GetData()
	if len(data) == 0 {
		return 0, fmt.Errorf("smart-turn output is empty")
	}
	return data[0], nil
}

// Close releases the onnxruntime session.
func (d *Detector) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		d.session.Destroy()
		d.session = nil
	}
}
