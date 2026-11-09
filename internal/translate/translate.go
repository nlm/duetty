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
	"unicode"

	"github.com/nlm/duetty/internal/apierr"
	"github.com/nlm/duetty/internal/config"
	"github.com/nlm/duetty/internal/usage"
	"github.com/nlm/duetty/internal/wordalign"
)

// Pair is a past conversation row, given to the translator as context.
// Lang is the language Original was spoken/typed in; Translation is in the
// other language of the pair.
type Pair struct{ Lang, Original, Translation string }

// Request is one translation job. History is oldest first.
type Request struct {
	Text, From, To string
	History        []Pair
	Correct        bool // ask for the corrected original too
}

// Result is a translation, plus the corrected original when asked for.
type Result struct {
	Translation string
	Corrected   string // "" when not asked, unusable or discarded
}

// Translator translates text from one language code to another.
type Translator interface {
	Translate(ctx context.Context, req Request) (Result, error)
}

// Client talks to {baseURL}/chat/completions. It has no global HTTP
// timeout; callers bound requests with ctx.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	meter   *usage.Meter
	context string
	words   map[string]bool // context words, for checking corrections
}

// NewClient builds a Client.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, http: &http.Client{}}
}

// SetMeter makes the client report token usage to m. Call it before the
// client is used concurrently.
func (c *Client) SetMeter(m *usage.Meter) { c.meter = m }

// SetContext sets the context body appended to the fixed instruction. Call
// it before the client is used concurrently.
func (c *Client) SetContext(body string) {
	c.context = body
	c.words = contextWords(body)
}

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
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"` // may be absent or null
	} `json:"usage"`
}

// instruction is the fixed, direction-independent start of the system message, so
// that it is byte-identical for every request and provider prompt caches hit.
const instruction = "You translate between two languages in a conversation. " +
	"Names, brands and terms follow the context below, when there is one. " +
	"When asked to correct the original, fix only words that speech recognition misheard; " +
	"never rephrase or drop content. " +
	"Use a context term only where the heard words sound like it; never swap one name for another, " +
	"and never add words the speaker didn't say, except to restore a clearly missing small word."

// systemPrompt is the start of the single system message: the instruction,
// then the context. It is the same for every request, so provider prompt
// caches hit.
func (c *Client) systemPrompt() string {
	if c.context == "" {
		return instruction
	}
	return instruction + "\n\nContext:\n" + c.context
}

// formatLine is the end of the system message: the direction and output
// format. Names are English language names.
func formatLine(from, to string, correct bool) string {
	s := fmt.Sprintf("Translate from %s to %s. ", config.EnglishName(from), config.EnglishName(to))
	if correct {
		return s + `Reply with JSON only: {"corrected": "<the sentence as the speaker said it, with misheard words fixed>", "translation": "<its translation>"}.`
	}
	return s + "Output only the translation, nothing else. No quotes, no notes."
}

// correctedReply is the JSON object asked for in correct mode.
type correctedReply struct {
	Corrected   string `json:"corrected"`
	Translation string `json:"translation"`
}

// messages builds the chat: one system message (several chat templates
// reject a second one) holding the shared prefix then the direction and
// format, each history row as a user message in the from language
// and an assistant message in the to language, then the text. In correct
// mode assistant turns are the JSON object, with corrected set to the user
// text.
func (c *Client) messages(req Request) []message {
	msgs := make([]message, 0, 2+2*len(req.History))
	msgs = append(msgs, message{"system", c.systemPrompt() + "\n\n" + formatLine(req.From, req.To, req.Correct)})
	for _, p := range req.History {
		user, assistant := p.Original, p.Translation
		if p.Lang == req.To { // spoken in the target language: swap sides
			user, assistant = p.Translation, p.Original
		}
		if req.Correct {
			b, _ := json.Marshal(correctedReply{Corrected: user, Translation: assistant})
			assistant = string(b)
		}
		msgs = append(msgs, message{"user", user}, message{"assistant", assistant})
	}
	return append(msgs, message{"user", req.Text})
}

// Translate implements Translator. Errors never contain the API key or the
// response body.
func (c *Client) Translate(ctx context.Context, req Request) (Result, error) {
	content, err := c.complete(ctx, c.messages(req))
	if err != nil {
		return Result{}, err
	}
	if !req.Correct {
		return Result{Translation: cleanReply(content, req.Text)}, nil
	}
	var r correctedReply
	if ExtractJSONObject(content, &r) != nil || strings.TrimSpace(r.Translation) == "" {
		// Never lose a sentence: ask again for the translation alone.
		return c.translateOnly(ctx, req, req.Text)
	}
	res := Result{Translation: cleanReply(r.Translation, req.Text)}
	corrected := strings.TrimSpace(r.Corrected)
	if corrected == "" || similarity(req.Text, corrected) < minSimilarity {
		return res, nil
	}
	checked, dropped := checkCorrection(req.Text, corrected, c.words)
	if !dropped {
		res.Corrected = corrected
		return res, nil
	}
	// A change was dropped: translate what is shown, so both sides match.
	if retried, err := c.translateOnly(ctx, req, checked); err == nil {
		if checked != wordalign.Join(wordalign.Tokens(req.Text)) {
			retried.Corrected = checked
		}
		return retried, nil
	}
	// Never lose a sentence: fall back to the heard text.
	return c.translateOnly(ctx, req, req.Text)
}

// translateOnly translates text with a translation-only request, keeping
// the history and direction of req.
func (c *Client) translateOnly(ctx context.Context, req Request, text string) (Result, error) {
	req.Text, req.Correct = text, false
	content, err := c.complete(ctx, c.messages(req))
	if err != nil {
		return Result{}, err
	}
	return Result{Translation: cleanReply(content, text)}, nil
}

// complete sends one chat request and returns the reply content.
func (c *Client) complete(ctx context.Context, msgs []message) (string, error) {
	body, err := json.Marshal(request{Model: c.model, Temperature: 0, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("encode translation request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" { // local servers take no key
		hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(hreq)
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
	if n := p.Usage.PromptTokensDetails.CachedTokens; n != 0 {
		c.meter.AddCached(n)
	}
	if len(p.Choices) == 0 {
		return "", errors.New("translation response: no choices")
	}
	return p.Choices[0].Message.Content, nil
}

// ExtractJSONObject unmarshals the JSON object found in s, from the first
// '{' to the last '}', into v. It tolerates prose and code fences around it.
func ExtractJSONObject(s string, v any) error {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return errors.New("no JSON object in reply")
	}
	return json.Unmarshal([]byte(s[i:j+1]), v)
}

// minSimilarity is the least similarity between the heard text and its
// correction; below it the correction is discarded as a rephrasing.
const minSimilarity = 0.5

// similarity is 1 - Levenshtein(a, b) / max(len(a), len(b)) over runes, on
// the texts case-folded with punctuation and whitespace removed.
func similarity(a, b string) float64 {
	ra, rb := normalize(a), normalize(b)
	n := max(len(ra), len(rb))
	if n == 0 {
		return 1
	}
	return 1 - float64(levenshtein(ra, rb))/float64(n)
}

// normalize lowercases s and drops punctuation and whitespace.
func normalize(s string) []rune {
	var out []rune
	for _, r := range strings.ToLower(s) {
		if !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			out = append(out, r)
		}
	}
	return out
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
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
