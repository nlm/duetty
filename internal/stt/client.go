// Package stt is a minimal client for an OpenAI-compatible Whisper
// /audio/transcriptions endpoint.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/nlm/duetty/internal/apierr"
	"github.com/nlm/duetty/internal/config"
	"github.com/nlm/duetty/internal/usage"
)

// Result is one transcription.
type Result struct {
	Text     string
	Language string // ISO-639-1 as returned; "" if absent
	// AvgLogprob is the token-weighted mean of segment avg_logprob; 0 when
	// there are no segments (so 0 means "unknown", not "perfect").
	AvgLogprob float64
	// CompressionRatio is the max over segments; 0 when there are no segments.
	CompressionRatio float64
	// Segments are Whisper's segments with timings in seconds on the uploaded
	// audio's timeline; text is trimmed.
	Segments []Segment
}

// Segment is one Whisper segment.
type Segment struct {
	Start, End float64
	Text       string
}

// Transcriber transcribes 16 kHz mono WAV audio. language "" = auto-detect.
type Transcriber interface {
	Transcribe(ctx context.Context, wav16k []byte, language string) (Result, error)
}

// Client talks to {baseURL}/audio/transcriptions. It has no global HTTP
// timeout; callers bound requests with ctx.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	meter   *usage.Meter
	hints   map[string]string
}

// NewClient builds a Client.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, http: &http.Client{}}
}

// SetMeter makes the client report billed audio seconds to m. Call it before
// the client is used concurrently.
func (c *Client) SetMeter(m *usage.Meter) { c.meter = m }

// SetHints sets the per-language Whisper prompt, keyed by language code. Call
// it before the client is used concurrently; nil means no hints.
func (c *Client) SetHints(h map[string]string) { c.hints = h }

type response struct {
	Language string `json:"language"`
	Text     string `json:"text"`
	Usage    struct {
		Type    string  `json:"type"`
		Seconds float64 `json:"seconds"`
	} `json:"usage"`
	Segments []struct {
		Start            float64 `json:"start"`
		End              float64 `json:"end"`
		Text             string  `json:"text"`
		AvgLogprob       float64 `json:"avg_logprob"`
		CompressionRatio float64 `json:"compression_ratio"`
		Tokens           []int   `json:"tokens"`
	} `json:"segments"`
}

// Transcribe implements Transcriber. Errors never contain the API key or
// the response body.
func (c *Client) Transcribe(ctx context.Context, wav16k []byte, language string) (Result, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fields := [][2]string{{"model", c.model}, {"response_format", "verbose_json"}, {"temperature", "0"}}
	if language != "" {
		fields = append(fields, [2]string{"language", language})
		if h := c.hints[language]; h != "" {
			fields = append(fields, [2]string{"prompt", h})
		}
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return Result{}, fmt.Errorf("write %s field: %w", f[0], err)
		}
	}
	part, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return Result{}, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(wav16k); err != nil {
		return Result{}, fmt.Errorf("write wav: %w", err)
	}
	if err := mw.Close(); err != nil {
		return Result{}, fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.apiKey != "" { // local servers take no key
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("transcription request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("read transcription response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, &apierr.StatusError{Op: "transcription", Code: resp.StatusCode, Status: resp.Status}
	}
	var p response
	if err := json.Unmarshal(raw, &p); err != nil {
		return Result{}, fmt.Errorf("parse transcription response: %w", err)
	}

	if p.Usage.Type == "duration" {
		c.meter.AddAudio(p.Usage.Seconds)
	}

	res := Result{Text: strings.TrimSpace(p.Text)}
	if l := strings.TrimSpace(p.Language); l != "" {
		if code, ok := config.LanguageCode(l); ok {
			res.Language = code
		} else {
			res.Language = strings.ToLower(l)
		}
	}
	var sum float64
	var n int
	for _, s := range p.Segments {
		res.Segments = append(res.Segments, Segment{Start: s.Start, End: s.End, Text: strings.TrimSpace(s.Text)})
	}
	for _, s := range p.Segments {
		w := len(s.Tokens)
		sum += s.AvgLogprob * float64(w)
		n += w
		if s.CompressionRatio > res.CompressionRatio {
			res.CompressionRatio = s.CompressionRatio
		}
	}
	if n > 0 {
		res.AvgLogprob = sum / float64(n)
	} else if len(p.Segments) > 0 { // segments without tokens: plain mean
		for _, s := range p.Segments {
			res.AvgLogprob += s.AvgLogprob
		}
		res.AvgLogprob /= float64(len(p.Segments))
	}
	return res, nil
}
