// Package apierr holds the error type shared by the HTTP API clients.
package apierr

// StatusError is a non-2xx HTTP response. It carries only the status line,
// never the response body or credentials, so it is safe to show to users.
type StatusError struct {
	Op     string // "transcription", "translation"
	Code   int    // 503
	Status string // "503 Service Unavailable"
}

func (e *StatusError) Error() string { return e.Op + ": " + e.Status }
