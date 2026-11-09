package translate

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/nlm/duetty/internal/usage"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nlm/duetty/internal/apierr"
)

type msg struct{ Role, Content string }

type reqBody struct {
	Model       string   `json:"model"`
	Temperature *float64 `json:"temperature"`
	Messages    []msg    `json:"messages"`
}

func serve(t *testing.T, status int, reply string, got *reqBody) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/chat/completions" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sekrit" {
			t.Error("bad auth header")
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type %q", ct)
		}
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("decode: %v", err)
			}
		}
		if status != 200 {
			http.Error(w, reply, status)
			return
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": reply}}}})
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/", "sekrit", "mistral-small")
}

const wantDirection = "Translate from French to English. Output only the translation, nothing else. No quotes, no notes."

func TestTranslateRequestShape(t *testing.T) {
	var body reqBody
	c := serve(t, 200, "  \"Good morning.\"\n", &body)
	hist := []Pair{
		{Lang: "fr", Original: "Salut", Translation: "Hi"},
		{Lang: "en", Original: "How are you?", Translation: "Comment ça va ?"},
	}
	got, err := c.Translate(context.Background(), Request{Text: "Bonjour.", From: "fr", To: "en", History: hist})
	if err != nil {
		t.Fatal(err)
	}
	if got.Translation != "Good morning." || got.Corrected != "" {
		t.Errorf("got %q", got)
	}
	if body.Model != "mistral-small" {
		t.Errorf("model %q", body.Model)
	}
	if body.Temperature == nil || *body.Temperature != 0 {
		t.Errorf("temperature must be present and 0: %v", body.Temperature)
	}
	want := []msg{
		{"system", instruction + "\n\n" + wantDirection},
		{"user", "Salut"}, {"assistant", "Hi"},
		// row spoken in the target language: user side is the French text
		{"user", "Comment ça va ?"}, {"assistant", "How are you?"},
		{"user", "Bonjour."},
	}
	if len(body.Messages) != len(want) {
		t.Fatalf("messages = %+v", body.Messages)
	}
	for i := range want {
		if body.Messages[i] != want[i] {
			t.Errorf("message %d = %+v want %+v", i, body.Messages[i], want[i])
		}
	}
	if !strings.Contains(body.Messages[0].Content, "French") || !strings.Contains(body.Messages[0].Content, "English") {
		t.Error("system prompt must use English language names")
	}
}

func TestTranslateContextZero(t *testing.T) {
	var body reqBody
	c := serve(t, 200, "Hello", &body)
	if _, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"}); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1] != (msg{"user", "Bonjour"}) {
		t.Errorf("messages = %+v", body.Messages)
	}
}

func TestTranslateHTTPError(t *testing.T) {
	c := serve(t, http.StatusUnauthorized, "secret-body-detail", nil)
	_, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"})
	var se *apierr.StatusError
	if !errors.As(err, &se) || se.Status != "401 Unauthorized" {
		t.Fatalf("err = %v", err)
	}
	if s := err.Error(); strings.Contains(s, "sekrit") || strings.Contains(s, "secret-body-detail") {
		t.Errorf("error leaks key or body: %q", s)
	}
}

func TestTranslateNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "k", "m").Translate(context.Background(), Request{Text: "x", From: "fr", To: "en"}); err == nil {
		t.Error("want error on empty choices")
	}
}

func TestCleanReply(t *testing.T) {
	for in, want := range map[string]string{
		"  Hello  ":    "Hello",
		`"Hello"`:      "Hello",
		"« Bonjour »":  "Bonjour",
		"“Hi there”\n": "Hi there",
		"'quoted'":     "quoted",
		"l'eau":        "l'eau",
		`He said "hi"`: `He said "hi"`,
		"「こんにちは」":      "こんにちは",
		`"A" and "B"`:  `"A" and "B"`,
		"«A» et «B»":   "«A» et «B»",
		`"'nested'"`:   "nested",
	} {
		if got := cleanReply(in, "source"); got != want {
			t.Errorf("cleanReply(%q) = %q want %q", in, got, want)
		}
	}
}

func TestCleanReplyKeepsQuotedSource(t *testing.T) {
	for _, c := range []struct{ reply, source, want string }{
		{`"A" et "B"`, `"A" and "B"`, `"A" et "B"`},
		{`"A" and "B"`, `"A" et "B"`, `"A" and "B"`},
		{"«Bonjour»", "«Bonjour»", "«Bonjour»"},
		{`"Hello"`, "«Bonjour»", `"Hello"`},
		{`"Hello"`, "Bonjour", "Hello"},
	} {
		if got := cleanReply(c.reply, c.source); got != c.want {
			t.Errorf("cleanReply(%q, source %q) = %q want %q", c.reply, c.source, got, c.want)
		}
	}
}

func usageServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "k", "m")
}

func TestTranslateCountsTokens(t *testing.T) {
	c := usageServer(t, `{"choices":[{"message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":23,"completion_tokens":9,"total_tokens":32}}`)
	m := usage.NewMeter(nil)
	c.SetMeter(m)
	if _, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"}); err != nil {
		t.Fatal(err)
	}
	if tot := m.Totals(); tot.InTokens != 23 || tot.OutTokens != 9 {
		t.Errorf("totals = %+v, want in=23 out=9", tot)
	}
}

func TestTranslateCountsWithoutChoices(t *testing.T) {
	c := usageServer(t, `{"choices":[],"usage":{"prompt_tokens":23,"completion_tokens":9}}`)
	m := usage.NewMeter(nil)
	c.SetMeter(m)
	if _, err := c.Translate(context.Background(), Request{Text: "x", From: "fr", To: "en"}); err == nil {
		t.Error("want error on empty choices")
	}
	if tot := m.Totals(); tot.InTokens != 23 || tot.OutTokens != 9 {
		t.Errorf("totals = %+v, want in=23 out=9", tot)
	}
}

func TestNoKeyNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("Authorization header sent without a key")
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello."}}]}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "", "m").Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"}); err != nil {
		t.Fatal(err)
	}
}

// multiServer replies with replies in order (the last one repeats) and
// records every request body.
func multiServer(t *testing.T, replies ...string) (*Client, *[]reqBody) {
	t.Helper()
	var bodies []reqBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b reqBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode: %v", err)
		}
		i := len(bodies)
		bodies = append(bodies, b)
		if i >= len(replies) {
			i = len(replies) - 1
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": replies[i]}}}})
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "k", "m"), &bodies
}

func TestPrefixStableAcrossDirections(t *testing.T) {
	c, bodies := multiServer(t, "ok")
	c.SetContext("CTX")
	h1 := []Pair{{Lang: "fr", Original: "Salut", Translation: "Hi"}}
	h2 := []Pair{{Lang: "en", Original: "a", Translation: "b"}, {Lang: "fr", Original: "c", Translation: "d"}}
	if _, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en", History: h1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Translate(context.Background(), Request{Text: "Hello", From: "en", To: "fr", History: h2, Correct: true}); err != nil {
		t.Fatal(err)
	}
	b := *bodies
	prefix := instruction + "\n\nContext:\nCTX"
	for i, body := range b {
		systems := 0
		for _, m := range body.Messages {
			if m.Role == "system" {
				systems++
			}
		}
		if systems != 1 || body.Messages[0].Role != "system" {
			t.Errorf("request %d: want exactly one system message, first: %+v", i, body.Messages)
		}
		if !strings.HasPrefix(body.Messages[0].Content, prefix+"\n\nTranslate from ") {
			t.Errorf("request %d system = %q", i, body.Messages[0].Content)
		}
	}
	if !strings.Contains(b[0].Messages[0].Content, "French to English") || !strings.Contains(b[1].Messages[0].Content, "English to French") {
		t.Error("direction missing from the system message")
	}
}

func TestInstructionWording(t *testing.T) {
	c, bodies := multiServer(t, "ok")
	if _, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en", Correct: true}); err != nil {
		t.Fatal(err)
	}
	sys := strings.ToLower((*bodies)[0].Messages[0].Content)
	if !strings.Contains(sys, "never rephrase") {
		t.Error("system message must forbid rephrasing")
	}
	for _, rule := range []string{"only where the heard words sound like it", "never swap one name for another", "clearly missing small word"} {
		if !strings.Contains(sys, rule) {
			t.Errorf("system message lacks %q", rule)
		}
	}
}

func TestNoContextNoContextLine(t *testing.T) {
	c, bodies := multiServer(t, "ok")
	if _, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*bodies)[0].Messages[0].Content, "Context:") {
		t.Error("unexpected Context: line")
	}
}

func correct(t *testing.T, heard string, replies ...string) (Result, []reqBody) {
	t.Helper()
	c, bodies := multiServer(t, replies...)
	res, err := c.Translate(context.Background(), Request{Text: heard, From: "en", To: "fr", Correct: true})
	if err != nil {
		t.Fatal(err)
	}
	return res, *bodies
}

func TestCorrectReply(t *testing.T) {
	res, bodies := correct(t, "scale way is great", `{"corrected":"Scaleway is great","translation":"Scaleway est super"}`)
	if res != (Result{"Scaleway est super", "Scaleway is great"}) {
		t.Errorf("res = %+v", res)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0].Messages[0].Content, "Reply with JSON only") {
		t.Errorf("bodies = %+v", bodies)
	}
}

func TestCorrectReplyFenced(t *testing.T) {
	res, _ := correct(t, "scale way is great", "Here you go:\n```json\n{\"corrected\":\"Scaleway is great\",\"translation\":\"Scaleway est super\"}\n```")
	if res != (Result{"Scaleway est super", "Scaleway is great"}) {
		t.Errorf("res = %+v", res)
	}
}

func TestCorrectMalformedFallsBack(t *testing.T) {
	res, bodies := correct(t, "hello", "oops", "Bonjour")
	if res != (Result{"Bonjour", ""}) {
		t.Errorf("res = %+v", res)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests", len(bodies))
	}
	if strings.Contains(bodies[1].Messages[0].Content, "JSON") || !strings.Contains(bodies[1].Messages[0].Content, "Output only the translation") {
		t.Errorf("second request format line: %q", bodies[1].Messages[0].Content)
	}
}

func TestCorrectEmptyTranslationFallsBack(t *testing.T) {
	res, bodies := correct(t, "hello", `{"corrected":"hello","translation":""}`, "Bonjour")
	if res != (Result{"Bonjour", ""}) || len(bodies) != 2 {
		t.Errorf("res = %+v, %d requests", res, len(bodies))
	}
}

func TestCorrectionTooDifferentDiscarded(t *testing.T) {
	res, _ := correct(t, "we ship on friday", `{"corrected":"the cat sat on the mat today","translation":"on expédie vendredi"}`)
	if res != (Result{"on expédie vendredi", ""}) {
		t.Errorf("res = %+v", res)
	}
}

func TestShortSplitWordKept(t *testing.T) {
	res, _ := correct(t, "scale way", `{"corrected":"Scaleway","translation":"Scaleway"}`)
	if res.Corrected != "Scaleway" {
		t.Errorf("res = %+v", res)
	}
}

func TestCorrectHistoryRenderedAsJSON(t *testing.T) {
	c, bodies := multiServer(t, `{"corrected":"x","translation":"y"}`)
	h := []Pair{{Lang: "en", Original: "Hi", Translation: "Salut"}}
	if _, err := c.Translate(context.Background(), Request{Text: "x", From: "en", To: "fr", History: h, Correct: true}); err != nil {
		t.Fatal(err)
	}
	m := (*bodies)[0].Messages
	if m[2].Role != "assistant" {
		t.Fatalf("messages = %+v", m)
	}
	var v struct{ Corrected, Translation string }
	if err := json.Unmarshal([]byte(m[2].Content), &v); err != nil || v.Corrected != "Hi" || v.Translation != "Salut" {
		t.Errorf("assistant turn %q (%v)", m[2].Content, err)
	}
}

func TestPlainModeUnchanged(t *testing.T) {
	c, bodies := multiServer(t, `"Hello"`)
	res, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"})
	if err != nil {
		t.Fatal(err)
	}
	m := (*bodies)[0].Messages
	if len(m) != 2 || !strings.Contains(m[0].Content, "Output only the translation") || res != (Result{"Hello", ""}) {
		t.Errorf("res = %+v, messages = %+v", res, m)
	}
}

func TestCachedTokensMetered(t *testing.T) {
	c := usageServer(t, `{"choices":[{"message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":23,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":7}}}`)
	m := usage.NewMeter(nil)
	c.SetMeter(m)
	if _, err := c.Translate(context.Background(), Request{Text: "Bonjour", From: "fr", To: "en"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Totals().CachedTokens; got != 7 {
		t.Errorf("cached = %d", got)
	}
}

func TestCachedTokensAbsentOrNull(t *testing.T) {
	for _, u := range []string{`{"prompt_tokens":1}`, `{"prompt_tokens":1,"prompt_tokens_details":null}`} {
		c := usageServer(t, `{"choices":[{"message":{"content":"Hello"}}],"usage":`+u+`}`)
		m := usage.NewMeter(nil)
		c.SetMeter(m)
		if _, err := c.Translate(context.Background(), Request{Text: "B", From: "fr", To: "en"}); err != nil {
			t.Fatal(err)
		}
		if got := m.Totals().CachedTokens; got != 0 {
			t.Errorf("cached = %d", got)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if s := similarity("scale way", "Scaleway"); s != 1 {
		t.Errorf("split word = %v", s)
	}
	if s := similarity("we ship on friday", "the cat sat on the mat today"); s >= 0.5 {
		t.Errorf("unrelated = %v", s)
	}
}

// failingSecond replies with replies in order but answers the second
// request with 500, and records every request body.
func failingSecond(t *testing.T, replies ...string) (*Client, *[]reqBody) {
	t.Helper()
	var bodies []reqBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b reqBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode: %v", err)
		}
		i := len(bodies)
		bodies = append(bodies, b)
		if i == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if i >= len(replies) {
			i = len(replies) - 1
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": replies[i]}}}})
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "k", "m"), &bodies
}

func TestDroppedChangeRetranslates(t *testing.T) {
	res, bodies := correct(t, "ask Tom about scale way",
		`{"corrected":"ask Dan about Scaleway","translation":"demande à Dan pour Scaleway"}`,
		"demande à Tom pour Scaleway")
	if res != (Result{"demande à Tom pour Scaleway", "ask Tom about Scaleway"}) {
		t.Errorf("res = %+v", res)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	m := bodies[1].Messages
	if strings.Contains(m[0].Content, "JSON") || !strings.Contains(m[0].Content, "Output only the translation") {
		t.Errorf("second request is not translation-only: %q", m[0].Content)
	}
	if last := m[len(m)-1]; last.Role != "user" || last.Content != "ask Tom about Scaleway" {
		t.Errorf("second request text = %+v", last)
	}
}

func TestKeptChangesNoExtraRequest(t *testing.T) {
	res, bodies := correct(t, "scale way is great", `{"corrected":"Scaleway is great","translation":"Scaleway est super"}`)
	if len(bodies) != 1 || res != (Result{"Scaleway est super", "Scaleway is great"}) {
		t.Errorf("res = %+v, %d requests", res, len(bodies))
	}
}

func TestRetranslateFailureNeverLoses(t *testing.T) {
	c, bodies := failingSecond(t,
		`{"corrected":"ask Dan about Scaleway","translation":"demande à Dan pour Scaleway"}`,
		"unused",
		"demande à Tom pour scale way")
	res, err := c.Translate(context.Background(), Request{Text: "ask Tom about scale way", From: "en", To: "fr", Correct: true})
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{"demande à Tom pour scale way", ""}) {
		t.Errorf("res = %+v", res)
	}
	b := *bodies
	if len(b) != 3 {
		t.Fatalf("%d requests, want 3", len(b))
	}
	if last := b[2].Messages[len(b[2].Messages)-1]; last.Content != "ask Tom about scale way" {
		t.Errorf("fallback text = %q, want the heard text", last.Content)
	}
}

func TestExtraHeardSpacesNoFakeCorrection(t *testing.T) {
	res, bodies := correct(t, "ask  Tom about  it",
		`{"corrected":"ask Dan about it","translation":"demande à Dan"}`,
		"demande à Tom")
	if res != (Result{"demande à Tom", ""}) || len(bodies) != 2 {
		t.Errorf("res = %+v, %d requests", res, len(bodies))
	}
}
