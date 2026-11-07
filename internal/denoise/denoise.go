// Package denoise is optional, local, real-time noise suppression for the mic
// path (config `denoise` / -denoise, off by default), run on 48 kHz audio
// before Silero and Whisper.
//
// The model is GTCRN (Rong et al., ICASSP 2024; github.com/Xiaobin-Rong/gtcrn,
// MIT), the streaming ONNX export from its repo (stream/onnx_models/
// gtcrn_simple.onnx, ~48k parameters, DNS3-trained), installed by
// scripts/setup-denoise.sh. It works at 16 kHz on one STFT frame at a time
// (512-point FFT, 256 hop, sqrt-Hann window) and threads three cache tensors
// between calls, so all DSP around it is small and lives here:
//
//	48 kHz s16le -> Decimator (16 kHz) -> STFT frame -> GTCRN -> iSTFT +
//	overlap-add -> Interpolator (48 kHz) -> s16le
//
// 16 kHz loses nothing downstream: Silero and Whisper both consume 16 kHz.
//
// Process always returns exactly as many bytes as it is given; the audio is
// delayed by LatencySamples (~33 ms at 48 kHz): one 256-sample hop of
// buffering, one hop of STFT overlap, and both resampling filters' delays.
// The delay is primed with silence, so nothing is held back and Close needs no
// flush.
package denoise

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
	"gonum.org/v1/gonum/dsp/fourier"

	"github.com/nlm/poly/internal/audioutil"
	"github.com/nlm/poly/internal/log"
	"github.com/nlm/poly/internal/onnxenv"
	"github.com/nlm/poly/internal/stderrguard"
)

const (
	// ModelFile is the model's file name under models/denoise/.
	ModelFile = "gtcrn_simple.onnx"

	nFFT  = 512
	hop   = nFFT / 2
	nBins = nFFT/2 + 1
	// block is one hop in 48 kHz samples: the unit the pipeline works in.
	block = hop * 3

	// LatencySamples is the constant delay (48 kHz samples) between input
	// and output: the primed block, the STFT overlap and both FIR delays.
	LatencySamples = block + audioutil.DecimatorDelay48 + 3*hop + audioutil.InterpolatorDelay48
)

// GTCRN streaming I/O (stream/gtcrn_stream.py in the GTCRN repo).
var (
	inNames  = []string{"mix", "conv_cache", "tra_cache", "inter_cache"}
	outNames = []string{"enh", "conv_cache_out", "tra_cache_out", "inter_cache_out"}
	shapes   = []ort.Shape{
		ort.NewShape(1, nBins, 1, 2),
		ort.NewShape(2, 1, 16, 16, 33),
		ort.NewShape(2, 3, 1, 1, 16),
		ort.NewShape(2, 1, 33, 16),
	}
)

// Denoiser runs GTCRN on a continuous 48 kHz stream. Process and Close are
// safe to call from different goroutines; after Close, Process passes audio
// through unchanged.
type Denoiser struct {
	mu      sync.Mutex
	session *ort.AdvancedSession
	ins     []*ort.Tensor[float32]
	outs    []*ort.Tensor[float32]
	p       *pipeline
}

// New loads the GTCRN model at modelPath. ortLib is the onnxruntime shared
// library ("" uses onnxruntime's own search).
func New(ortLib, modelPath string) (*Denoiser, error) {
	if err := onnxenv.Ensure(ortLib); err != nil {
		return nil, fmt.Errorf("initialize onnxruntime environment: %w", err)
	}
	if err := checkModel(modelPath); err != nil {
		return nil, err
	}
	d := &Denoiser{}
	ok := false
	defer func() {
		if !ok {
			d.release()
		}
	}()
	for _, s := range shapes {
		in, err := ort.NewEmptyTensor[float32](s)
		if err != nil {
			return nil, fmt.Errorf("denoise input tensor: %w", err)
		}
		d.ins = append(d.ins, in)
		out, err := ort.NewEmptyTensor[float32](s)
		if err != nil {
			return nil, fmt.Errorf("denoise output tensor: %w", err)
		}
		d.outs = append(d.outs, out)
	}
	opts, err := onnxenv.NewSessionOptions("")
	if err != nil {
		return nil, fmt.Errorf("denoise session options: %w", err)
	}
	defer opts.Destroy()
	// ~62 tiny inferences per second: a thread pool costs more than it saves
	// (same reasoning as silerovad).
	if err := opts.SetIntraOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("denoise intra-op threads: %w", err)
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("denoise inter-op threads: %w", err)
	}
	ins := make([]ort.Value, len(d.ins))
	outs := make([]ort.Value, len(d.outs))
	for i := range d.ins {
		ins[i], outs[i] = d.ins[i], d.outs[i]
	}
	d.session, err = ort.NewAdvancedSession(modelPath, inNames, outNames, ins, outs, opts)
	if err != nil {
		return nil, fmt.Errorf("load denoise model %q: %w", modelPath, err)
	}
	d.p = newPipeline(d.enhance)
	ok = true
	return d, nil
}

// checkModel verifies modelPath has GTCRN's streaming I/O signature.
func checkModel(modelPath string) error {
	ins, outs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return fmt.Errorf("read denoise model %q metadata: %w", modelPath, err)
	}
	match := func(infos []ort.InputOutputInfo, names []string) bool {
		if len(infos) != len(names) {
			return false
		}
		for i, info := range infos {
			if info.Name != names[i] || info.DataType != ort.TensorElementDataTypeFloat ||
				info.Dimensions.FlattenedSize() != shapes[i].FlattenedSize() {
				return false
			}
		}
		return true
	}
	if !match(ins, inNames) || !match(outs, outNames) {
		return fmt.Errorf("denoise model %q is not a GTCRN streaming export (inputs %v, outputs %v)", modelPath, ins, outs)
	}
	return nil
}

// enhance runs one frame through GTCRN in place (spec is [257][2] re/im)
// and threads the caches. On error spec is left untouched (pass-through).
func (d *Denoiser) enhance(spec []float32) error {
	copy(d.ins[0].GetData(), spec)
	var err error
	stderrguard.Suppress(func() { err = d.session.Run() })
	if err != nil {
		return err
	}
	copy(spec, d.outs[0].GetData())
	for i := 1; i < len(d.ins); i++ {
		copy(d.ins[i].GetData(), d.outs[i].GetData())
	}
	return nil
}

// Process denoises a chunk of 48 kHz s16le mono PCM of any size and returns
// the same number of bytes, delayed by LatencySamples. A frame whose
// inference fails is passed through unchanged (the first failure is logged).
func (d *Denoiser) Process(chunk []byte) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return chunk
	}
	return d.p.process(chunk)
}

// Close releases the model. Further Process calls pass audio through.
func (d *Denoiser) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.release()
}

func (d *Denoiser) release() {
	if d.session != nil {
		d.session.Destroy()
		d.session = nil
	}
	for _, t := range append(d.ins, d.outs...) {
		t.Destroy()
	}
	d.ins, d.outs = nil, nil
}

// Wrap returns a channel that applies Process to every chunk from in. It is
// closed when in is closed.
func Wrap(in <-chan []byte, d *Denoiser) <-chan []byte {
	out := make(chan []byte, cap(in))
	go func() {
		defer close(out)
		for c := range in {
			out <- d.Process(c)
		}
	}()
	return out
}

// pipeline is the DSP around the model, separate from ONNX so it can be
// tested with a pass-through enhancer.
type pipeline struct {
	enhance func(spec []float32) error
	failed  bool // an inference error was already logged

	carry  []byte    // odd trailing input byte
	in48   []float64 // input not yet making a full block
	dec    *audioutil.Decimator
	interp *audioutil.Interpolator
	fft    *fourier.FFT
	window []float64 // sqrt periodic Hann
	frame  []float64 // last nFFT samples at 16 kHz
	tail   []float64 // overlap-add tail of the previous frame (hop samples)
	buf    []float64
	coef   []complex128
	spec   []float32
	out    []byte // output FIFO (s16le), primed with one block of silence
}

func newPipeline(enhance func([]float32) error) *pipeline {
	w := make([]float64, nFFT)
	for i := range w {
		w[i] = math.Sqrt(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/nFFT))
	}
	return &pipeline{
		enhance: enhance,
		dec:     audioutil.NewDecimator48to16(),
		interp:  audioutil.NewInterpolator16to48(),
		fft:     fourier.NewFFT(nFFT),
		window:  w,
		frame:   make([]float64, nFFT),
		tail:    make([]float64, hop),
		buf:     make([]float64, nFFT),
		coef:    make([]complex128, nBins),
		spec:    make([]float32, 2*nBins),
		out:     make([]byte, 2*block),
	}
}

func (p *pipeline) process(chunk []byte) []byte {
	b := chunk
	if len(p.carry) > 0 {
		b = append(append([]byte{}, p.carry...), chunk...)
		p.carry = p.carry[:0]
	}
	if len(b)%2 == 1 {
		p.carry = append(p.carry, b[len(b)-1])
		b = b[:len(b)-1]
	}
	for i := 0; i+1 < len(b); i += 2 {
		p.in48 = append(p.in48, float64(int16(binary.LittleEndian.Uint16(b[i:])))/32768)
	}
	n := 0
	for ; n+block <= len(p.in48); n += block {
		p.block(p.in48[n : n+block])
	}
	p.in48 = append(p.in48[:0], p.in48[n:]...)

	// Invariant: the FIFO always holds at least len(chunk) bytes (it is
	// primed with one block and every full block of input adds one).
	res := make([]byte, len(chunk))
	copy(res, p.out)
	p.out = append(p.out[:0], p.out[len(chunk):]...)
	return res
}

// block denoises one hop (block 48 kHz samples) and appends it to the FIFO.
func (p *pipeline) block(in []float64) {
	s16 := p.dec.Process(in) // exactly hop samples
	copy(p.frame, p.frame[hop:])
	copy(p.frame[nFFT-hop:], s16)
	for i, v := range p.frame {
		p.buf[i] = v * p.window[i]
	}
	p.fft.Coefficients(p.coef, p.buf)
	for k, c := range p.coef {
		p.spec[2*k], p.spec[2*k+1] = float32(real(c)), float32(imag(c))
	}
	if err := p.enhance(p.spec); err != nil {
		if !p.failed {
			p.failed = true
			log.Warnf("denoise: inference failed, passing audio through: %v", err)
		}
	} else {
		for k := range p.coef {
			p.coef[k] = complex(float64(p.spec[2*k]), float64(p.spec[2*k+1]))
		}
	}
	p.fft.Sequence(p.buf, p.coef)
	y := make([]float64, hop)
	for i := 0; i < nFFT; i++ {
		v := p.buf[i] / nFFT * p.window[i]
		if i < hop {
			y[i] = p.tail[i] + v
		} else {
			p.tail[i-hop] = v
		}
	}
	for _, v := range p.interp.Process(y) {
		s := math.Round(v * 32768)
		s = math.Max(-32768, math.Min(32767, s))
		p.out = binary.LittleEndian.AppendUint16(p.out, uint16(int16(s)))
	}
}
