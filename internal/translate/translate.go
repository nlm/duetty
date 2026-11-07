// Package translate is a minimal client for an OpenAI-compatible
// /chat/completions endpoint, used as a translator.
package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nlm/poly/internal/apierr"
	"github.com/nlm/poly/internal/config"
	"github.com/nlm/poly/internal/usage"
)

// Pair is a past conversation row, given to the translator as context.
// Lang is the language Original was spoken/typed in; Translation is in the
// other language of the pair.
type Pair struct{ Lang, Original, Translation string }

// Translator translates text from one language code to another. history is
// oldest first.
type Translator interface {
	Translate(ctx context.Context, text, from, to string, history []Pair) (string, error)
}

// Client talks to {baseURL}/chat/completions. It has no global HTTP
// timeout; callers bound requests with ctx.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	meter   *usage.Meter
}

// NewClient builds a Client.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, http: &http.Client{}}
}

// SetMeter makes the client report token usage to m. Call it before the
// client is used concurrently.
func (c *Client) SetMeter(m *usage.Meter) { c.meter = m }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model       string    `json:"model"`
	Temperature float64   `json:"temperature"` // no omitempty: 0 must be sent
	Messages    []message `json:"messages"`
}

type response struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// systemPrompt is the fixed instruction; names are English language names.
func systemPrompt(from, to string) string {
	return fmt.Sprintf("You translate from %s to %s. Output only the translation of the last user message, nothing else. No quotes, no notes.",
		config.EnglishName(from), config.EnglishName(to))
}

// messages builds the chat: system, then each history row as a user message
// in the from language and an assistant message in the to language, then text.
func messages(text, from, to string, history []Pair) []message {
	msgs := make([]message, 0, 2+2*len(history))
	msgs = append(msgs, message{"system", systemPrompt(from, to)})
	for _, p := range history {
		user, assistant := p.Original, p.Translation
		if p.Lang == to { // spoken in the target language: swap sides
			user, assistant = p.Translation, p.Original
		}
		msgs = append(msgs, message{"user", user}, message{"assistant", assistant})
	}
	return append(msgs, message{"user", text})
}

// Translate implements Translator. Errors never contain the API key or the
// response body.
func (c *Client) Translate(ctx context.Context, text, from, to string, history []Pair) (string, error) {
	body, err := json.Marshal(request{Model: c.model, Temperature: 0, Messages: messages(text, from, to, history)})
	if err != nil {
		return "", fmt.Errorf("encode translation request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("translation request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read translation response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &apierr.StatusError{Op: "translation", Code: resp.StatusCode, Status: resp.Status}
	}
	var p response
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errors.New("parse translation response: invalid JSON")
	}
	c.meter.AddTokens(p.Usage.PromptTokens, p.Usage.CompletionTokens) // billed even if unusable
	if len(p.Choices) == 0 {
		return "", errors.New("translation response: no choices")
	}
	return cleanReply(p.Choices[0].Message.Content, text), nil
}

// quotePairs are opening/closing quotes a model may wrap a reply in.
var quotePairs = [][2]string{
	{`"`, `"`}, {"'", "'"}, {"“", "”"}, {"„", "“"}, {"«", "»"}, {"‘", "’"}, {"「", "」"}, {"『", "』"},
}

// wrapped returns the quote pair s is wrapped in as a single outer pair
// (the closing quote does not also occur inside, as in `"A" et "B"`).
func wrapped(s string) ([2]string, bool) {
	for _, q := range quotePairs {
		if len(s) < len(q[0])+len(q[1]) || !strings.HasPrefix(s, q[0]) || !strings.HasSuffix(s, q[1]) {
			continue
		}
		inner := s[len(q[0]) : len(s)-len(q[1])]
		if strings.Contains(inner, q[1]) || strings.Contains(inner, q[0]) {
			continue
		}
		return q, true
	}
	return [2]string{}, false
}

// cleanReply strips surrounding whitespace and matching surrounding quotes
// the model added. Quotes are kept when the source text was itself quoted
// (any quote style: the translation may legitimately change it), and when
// the outer quotes are not a single pair (`"A" and "B"`).
func cleanReply(s, source string) string {
	s = strings.TrimSpace(s)
	if _, ok := wrapped(strings.TrimSpace(source)); ok {
		return s
	}
	for {
		q, ok := wrapped(s)
		if !ok {
			return s
		}
		s = strings.TrimSpace(s[len(q[0]) : len(s)-len(q[1])])
	}
}
