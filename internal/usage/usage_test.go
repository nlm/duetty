package usage

import "testing"

func TestFormatNoPrice(t *testing.T) {
	got := Format(Totals{AudioSeconds: 250, InTokens: 15000, OutTokens: 3300}, nil)
	if got != "18.3k tok · 4m10s audio" {
		t.Fatal(got)
	}
}

func TestFormatWithPrice(t *testing.T) {
	p := &Prices{AudioPerMin: 0.003, InPerMTok: 0.15, OutPerMTok: 0.35, Currency: "€"}
	got := Format(Totals{AudioSeconds: 250, InTokens: 15000, OutTokens: 3300}, p)
	if got != "≈ €0.016 · 18.3k tok · 4m10s audio" {
		t.Fatal(got)
	}
}

func TestFormatSmallAndLarge(t *testing.T) {
	cases := map[Totals]string{
		{}: "0 tok · 0s audio",
		{AudioSeconds: 42.4, InTokens: 600, OutTokens: 250}:           "850 tok · 42s audio",
		{AudioSeconds: 3725, InTokens: 1_500_000, OutTokens: 250_000}: "1.75M tok · 1h02m audio",
	}
	for in, want := range cases {
		if got := Format(in, nil); got != want {
			t.Errorf("%v: %q want %q", in, got, want)
		}
	}
}

func TestMeterAccumulatesAndNotifies(t *testing.T) {
	n := 0
	m := NewMeter(func() { n++ })
	m.AddAudio(3)
	m.AddTokens(23, 9)
	m.AddTokens(1, 1)
	if got := m.Totals(); got != (Totals{AudioSeconds: 3, InTokens: 24, OutTokens: 10}) || n != 3 {
		t.Fatal(got, n)
	}
	var nilM *Meter
	nilM.AddAudio(1)
	nilM.AddTokens(1, 1)
	if nilM.Totals() != (Totals{}) {
		t.Fatal("nil meter")
	}
}

func TestMeterSetOnChange(t *testing.T) {
	n := 0
	m := NewMeter(nil)
	m.AddTokens(1, 1) // no callback yet
	m.SetOnChange(func() { n++ })
	m.AddAudio(1)
	if n != 1 {
		t.Fatalf("callback ran %d times, want 1", n)
	}
	var nilM *Meter
	nilM.SetOnChange(func() {})
}
