package stt

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nlm/poly/internal/apierr"
)

func TestTranscribeHTTPErrorIsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "k", "m").Transcribe(context.Background(), []byte("x"), "")
	var se *apierr.StatusError
	if !errors.As(err, &se) || se.Status != "503 Service Unavailable" {
		t.Fatalf("err = %v", err)
	}
}
