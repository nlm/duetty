// Package usage tracks session audio and token usage and formats it for display.
package usage

import (
	"fmt"
	"math"
	"sync"
)

// Totals is a snapshot of accumulated session usage.
type Totals struct {
	AudioSeconds float64
	InTokens     int64
	OutTokens    int64
}

// Meter accumulates usage. All methods are safe for concurrent use and on a nil *Meter.
type Meter struct {
	mu       sync.Mutex
	totals   Totals
	onChange func()
}

// NewMeter returns a Meter that calls onChange (may be nil) after each update.
func NewMeter(onChange func()) *Meter {
	return &Meter{onChange: onChange}
}

// AddAudio adds sec seconds of audio.
func (m *Meter) AddAudio(sec float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.totals.AudioSeconds += sec
	m.mu.Unlock()
	m.notify()
}

// AddTokens adds input and output token counts.
func (m *Meter) AddTokens(in, out int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.totals.InTokens += in
	m.totals.OutTokens += out
	m.mu.Unlock()
	m.notify()
}

// Totals returns a snapshot of the accumulated usage.
func (m *Meter) Totals() Totals {
	if m == nil {
		return Totals{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totals
}

// SetOnChange replaces the function called after each update (nil: none).
func (m *Meter) SetOnChange(f func()) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onChange = f
	m.mu.Unlock()
}

func (m *Meter) notify() {
	m.mu.Lock()
	f := m.onChange
	m.mu.Unlock()
	if f != nil {
		f()
	}
}

// Prices holds per-unit prices used to estimate cost.
type Prices struct {
	AudioPerMin float64
	InPerMTok   float64
	OutPerMTok  float64
	Currency    string
}

// Format renders usage as a one-line status string. If p is non-nil, a cost
// estimate is prefixed.
func Format(t Totals, p *Prices) string {
	tok := formatTokens(t.InTokens + t.OutTokens)
	audio := formatDuration(t.AudioSeconds)
	body := fmt.Sprintf("%s tok · %s audio", tok, audio)
	if p == nil {
		return body
	}
	cost := t.AudioSeconds/60*p.AudioPerMin +
		float64(t.InTokens)/1e6*p.InPerMTok +
		float64(t.OutTokens)/1e6*p.OutPerMTok
	return fmt.Sprintf("≈ %s%.3f · ", p.Currency, cost) + body
}

func formatTokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	}
}

func formatDuration(sec float64) string {
	s := int64(math.Round(sec))
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}
