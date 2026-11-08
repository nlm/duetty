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

const wantSystem = "You translate from French to English. Output only the translation of the last user message, nothing else. No quotes, no notes."

func TestTranslateRequestShape(t *testing.T) {
	var body reqBody
	c := serve(t, 200, "  \"Good morning.\"\n", &body)
	hist := []Pair{
		{Lang: "fr", Original: "Salut", Translation: "Hi"},
		{Lang: "en", Original: "How are you?", Translation: "Comment ça va ?"},
	}
	got, err := c.Translate(context.Background(), "Bonjour.", "fr", "en", hist)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Good morning." {
		t.Errorf("got %q", got)
	}
	if body.Model != "mistral-small" {
		t.Errorf("model %q", body.Model)
	}
	if body.Temperature == nil || *body.Temperature != 0 {
		t.Errorf("temperature must be present and 0: %v", body.Temperature)
	}
	want := []msg{
		{"system", wantSystem},
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
	if _, err := c.Translate(context.Background(), "Bonjour", "fr", "en", nil); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1] != (msg{"user", "Bonjour"}) {
		t.Errorf("messages = %+v", body.Messages)
	}
}

func TestTranslateHTTPError(t *testing.T) {
	c := serve(t, http.StatusUnauthorized, "secret-body-detail", nil)
	_, err := c.Translate(context.Background(), "Bonjour", "fr", "en", nil)
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
	if _, err := NewClient(srv.URL, "k", "m").Translate(context.Background(), "x", "fr", "en", nil); err == nil {
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
	if _, err := c.Translate(context.Background(), "Bonjour", "fr", "en", nil); err != nil {
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
	if _, err := c.Translate(context.Background(), "x", "fr", "en", nil); err == nil {
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
	if _, err := NewClient(srv.URL, "", "m").Translate(context.Background(), "Bonjour", "fr", "en", nil); err != nil {
		t.Fatal(err)
	}
}
