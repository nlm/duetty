package onnxenv

import (
	"strings"
	"testing"
)

// TestEnsureOnlyInitializesOnce checks the core contract of this package:
// no matter how many times or with what libPath Ensure is called, the
// underlying onnxruntime_go environment is initialized at most once per
// process, and every call after the first returns the exact same result
// (here, in this test environment with no real onnxruntime library
// present, an error) rather than attempting -- and failing differently --
// again.
func TestEnsureOnlyInitializesOnce(t *testing.T) {
	err1 := Ensure("/nonexistent/libonnxruntime.so")
	err2 := Ensure("/a/different/nonexistent/path.so")
	err3 := Ensure("")

	if err1 == nil {
		t.Skip("onnxruntime environment initialized successfully in this environment; nothing to assert on the error path")
	}
	if err2 != err1 {
		t.Errorf("second Ensure call returned a different error: %v, want the cached %v", err2, err1)
	}
	if err3 != err1 {
		t.Errorf("third Ensure call (different libPath) returned a different error: %v, want the cached %v", err3, err1)
	}
}

// TestNewSessionOptionsRejectsUnknownProvider checks provider-name
// validation happens before ever touching onnxruntime -- deliberately, so
// this assertion holds regardless of whether Ensure has succeeded in this
// process (see TestEnsureOnlyInitializesOnce, which may have already
// permanently poisoned the shared once.Do with a deliberately-broken path,
// making onnxruntime itself unusable for the rest of this test binary).
func TestNewSessionOptionsRejectsUnknownProvider(t *testing.T) {
	_, err := NewSessionOptions("cuda")
	if err == nil {
		t.Fatal(`NewSessionOptions("cuda") should error -- only "", "cpu", and "coreml" are supported`)
	}
	if !strings.Contains(err.Error(), "cuda") {
		t.Errorf("error = %q, want it to name the rejected provider", err)
	}
}
