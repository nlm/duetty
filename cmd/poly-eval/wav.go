package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// wavPCM48k returns the sample bytes of a 48 kHz mono 16-bit PCM WAV file. It
// walks the RIFF chunks (fmt, then data), so extra chunks such as LIST/INFO
// written by ffmpeg without -bitexact are skipped rather than read as audio.
func wavPCM48k(b []byte) ([]byte, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	var haveFmt bool
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 16 || body+size > len(b) {
				return nil, errors.New("bad fmt chunk")
			}
			f := b[body : body+size]
			format := binary.LittleEndian.Uint16(f[0:2])
			channels := binary.LittleEndian.Uint16(f[2:4])
			rate := binary.LittleEndian.Uint32(f[4:8])
			bits := binary.LittleEndian.Uint16(f[14:16])
			if (format != 1 && format != 0xFFFE) || channels != 1 || rate != 48000 || bits != 16 {
				return nil, fmt.Errorf("want 48 kHz mono s16le PCM, got format %d, %d ch, %d Hz, %d bit", format, channels, rate, bits)
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, errors.New("data chunk before fmt chunk")
			}
			end := body + size
			if size == 0xFFFFFFFF || end > len(b) { // streamed / truncated: take the rest
				end = len(b)
			}
			data := b[body:end]
			return data[:len(data)&^1], nil
		}
		off = body + size + size&1 // chunks are word-aligned
	}
	return nil, errors.New("no data chunk")
}
