package stt

import (
	"context"
	"encoding/binary"
	"github.com/nlm/duetty/internal/usage"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const twoSeg = `{"language":"fr","text":" Bonjour.","segments":[{"avg_logprob":-0.2,"compression_ratio":1.1,"tokens":[1,2]},{"avg_logprob":-0.5,"compression_ratio":1.3,"tokens":[1,2,3,4]}]}`

func serve(t *testing.T, body string, check func(r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" || r.Method != "POST" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sekrit" {
			t.Errorf("bad auth header")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart: %v", err)
		}
		if check != nil {
			check(r)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "sekrit", "whisper-large-v3")
}

func TestTranscribeAutoSendsNoLanguage(t *testing.T) {
	c := serve(t, twoSeg, func(r *http.Request) {
		if _, ok := r.MultipartForm.Value["language"]; ok {
			t.Error("language field must be absent")
		}
		v := r.MultipartForm.Value
		if v["model"][0] != "whisper-large-v3" || v["response_format"][0] != "verbose_json" || v["temperature"][0] != "0" {
			t.Errorf("fields: %v", v)
		}
		if f := r.MultipartForm.File["file"]; len(f) != 1 || f[0].Filename != "audio.wav" {
			t.Errorf("file: %v", f)
		}
	})
	got, err := c.Transcribe(context.Background(), []byte("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Bonjour." || got.Language != "fr" || !near(got.AvgLogprob, -0.4) || got.CompressionRatio != 1.3 {
		t.Errorf("got %+v", got)
	}
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func TestTranscribeForcedSendsLanguage(t *testing.T) {
	c := serve(t, twoSeg, func(r *http.Request) {
		if r.MultipartForm.Value["language"][0] != "ja" {
			t.Errorf("language = %v", r.MultipartForm.Value["language"])
		}
	})
	if _, err := c.Transcribe(context.Background(), []byte("x"), "ja"); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeNullFields(t *testing.T) {
	c := serve(t, `{"language":"en","text":" Hi","segments":[{"avg_logprob":-0.1,"compression_ratio":1,"no_speech_prob":null,"tokens":[1]}],"words":null}`, nil)
	got, err := c.Transcribe(context.Background(), []byte("x"), "")
	if err != nil || got.Text != "Hi" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTranscribeEmptySegments(t *testing.T) {
	c := serve(t, `{"language":"en","text":" Hi","segments":[]}`, nil)
	got, err := c.Transcribe(context.Background(), []byte("x"), "")
	if err != nil || got.AvgLogprob != 0 || got.CompressionRatio != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTranscribeLanguageNameNormalized(t *testing.T) {
	c := serve(t, `{"language":"french","text":" Bonjour","segments":[]}`, nil)
	got, _ := c.Transcribe(context.Background(), []byte("x"), "")
	if got.Language != "fr" {
		t.Errorf("language = %q", got.Language)
	}
}

func TestTranscribeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "sekrit", "m").Transcribe(context.Background(), []byte("x"), "")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "sekrit") {
		t.Errorf("err = %v", err)
	}
}

func TestPCM48kToWAV16k(t *testing.T) {
	pcm := make([]byte, 48000*2)
	for i := 0; i < 48000; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(i%100*50)))
	}
	wav, err := PCM48kToWAV16k(pcm)
	if err != nil {
		t.Fatal(err)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Fatal("bad header")
	}
	le := binary.LittleEndian
	if le.Uint16(wav[20:]) != 1 || le.Uint16(wav[22:]) != 1 || le.Uint32(wav[24:]) != 16000 || le.Uint16(wav[34:]) != 16 {
		t.Error("bad fmt chunk")
	}
	if n := le.Uint32(wav[40:]); n != 32000 || len(wav) != 44+32000 || le.Uint32(wav[4:]) != uint32(len(wav)-8) {
		t.Errorf("data size %d, len %d", n, len(wav))
	}
}

func TestTranscribeCountsAudioSeconds(t *testing.T) {
	c := serve(t, `{"text":"hi","language":"en","usage":{"type":"duration","seconds":3}}`, nil)
	m := usage.NewMeter(nil)
	c.SetMeter(m)
	if _, err := c.Transcribe(context.Background(), []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if got := m.Totals().AudioSeconds; got != 3 {
		t.Errorf("AudioSeconds = %v, want 3", got)
	}
}

func TestTranscribeWithoutUsageCountsNothing(t *testing.T) {
	for name, body := range map[string]string{
		"absent":       `{"text":"hi","language":"en"}`,
		"non-duration": `{"text":"hi","usage":{"type":"tokens","seconds":9}}`,
	} {
		c := serve(t, body, nil)
		m := usage.NewMeter(nil)
		c.SetMeter(m)
		if _, err := c.Transcribe(context.Background(), []byte("x"), ""); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := m.Totals().AudioSeconds; got != 0 {
			t.Errorf("%s: AudioSeconds = %v, want 0", name, got)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"usage":{"type":"duration","seconds":3}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k", "m")
	m := usage.NewMeter(nil)
	c.SetMeter(m)
	if _, err := c.Transcribe(context.Background(), []byte("x"), ""); err == nil {
		t.Fatal("want error on 500")
	}
	if got := m.Totals().AudioSeconds; got != 0 {
		t.Errorf("non-2xx: AudioSeconds = %v, want 0", got)
	}
}

func TestTranscribeParsesSegments(t *testing.T) {
	c := serve(t, `{"language":"en","text":" Tom is angry. Don't.","segments":[{"start":0,"end":2.6,"text":" Tom is angry."},{"start":3.2,"end":4.9,"text":" Don't."}]}`, nil)
	res, err := c.Transcribe(context.Background(), []byte("x"), "")
	want := []Segment{{0, 2.6, "Tom is angry."}, {3.2, 4.9, "Don't."}}
	if err != nil || len(res.Segments) != 2 || res.Segments[0] != want[0] || res.Segments[1] != want[1] {
		t.Fatalf("got %+v %v", res.Segments, err)
	}
}
