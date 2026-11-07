package stt

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/nlm/poly/internal/audioutil"
)

// PCM48kToWAV16k converts 48 kHz s16le mono PCM to a 16 kHz mono 16-bit WAV.
func PCM48kToWAV16k(pcm []byte) ([]byte, error) {
	in := audioutil.BytesToInt16LE(pcm)
	f := make([]float64, len(in))
	for i, s := range in {
		f[i] = float64(s) / 32768
	}
	out, err := audioutil.ResampleTo16kHz(f, 48000)
	if err != nil {
		return nil, fmt.Errorf("resample: %w", err)
	}
	dataLen := len(out) * 2
	wav := make([]byte, 44+dataLen)
	le := binary.LittleEndian
	copy(wav[0:], "RIFF")
	le.PutUint32(wav[4:], uint32(36+dataLen))
	copy(wav[8:], "WAVEfmt ")
	le.PutUint32(wav[16:], 16)
	le.PutUint16(wav[20:], 1) // PCM
	le.PutUint16(wav[22:], 1) // mono
	le.PutUint32(wav[24:], 16000)
	le.PutUint32(wav[28:], 16000*2)
	le.PutUint16(wav[32:], 2)
	le.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	le.PutUint32(wav[40:], uint32(dataLen))
	for i, v := range out {
		s := math.Round(v * 32768)
		s = math.Max(-32768, math.Min(32767, s))
		le.PutUint16(wav[44+2*i:], uint16(int16(s)))
	}
	return wav, nil
}
