package apierr

import (
	"errors"
	"fmt"
	"testing"
)

func TestStatusError(t *testing.T) {
	var err error = fmt.Errorf("wrap: %w", &StatusError{Op: "translation", Code: 503, Status: "503 Service Unavailable"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("As failed: %v", err)
	}
	if got := se.Error(); got != "translation: 503 Service Unavailable" {
		t.Errorf("Error() = %q", got)
	}
}
