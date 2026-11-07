package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func chunk(id string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	if len(body)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

func fmtChunk(rate uint32, ch, bits uint16) []byte {
	f := make([]byte, 16)
	binary.LittleEndian.PutUint16(f[0:], 1)
	binary.LittleEndian.PutUint16(f[2:], ch)
	binary.LittleEndian.PutUint32(f[4:], rate)
	binary.LittleEndian.PutUint32(f[8:], rate*uint32(ch)*uint32(bits)/8)
	binary.LittleEndian.PutUint16(f[12:], ch*bits/8)
	binary.LittleEndian.PutUint16(f[14:], bits)
	return chunk("fmt ", f)
}

func riff(chunks ...[]byte) []byte {
	body := append([]byte("WAVE"), bytes.Join(chunks, nil)...)
	return chunk("RIFF", body)
}

func TestWavPCM48k(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6}
	// Plain 44-byte header.
	plain := riff(fmtChunk(48000, 1, 16), chunk("data", pcm))
	if len(plain) != 44+len(pcm) {
		t.Fatalf("plain header is %d bytes", len(plain)-len(pcm))
	}
	// ffmpeg without -bitexact: a LIST chunk (odd size, padded) before data.
	withList := riff(fmtChunk(48000, 1, 16), chunk("LIST", []byte("INFOISFT\x05\x00\x00\x00Lavf\x00")), chunk("data", pcm))
	for name, b := range map[string][]byte{"plain": plain, "list": withList} {
		got, err := wavPCM48k(b)
		if err != nil || !bytes.Equal(got, pcm) {
			t.Errorf("%s: got %v %v, want %v", name, got, err, pcm)
		}
	}
	for name, b := range map[string][]byte{
		"not riff":  []byte("RIFX0000WAVE"),
		"16k":       riff(fmtChunk(16000, 1, 16), chunk("data", pcm)),
		"stereo":    riff(fmtChunk(48000, 2, 16), chunk("data", pcm)),
		"no data":   riff(fmtChunk(48000, 1, 16)),
		"no fmt":    riff(chunk("data", pcm)),
		"truncated": riff(fmtChunk(48000, 1, 16))[:20],
	} {
		if _, err := wavPCM48k(b); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
