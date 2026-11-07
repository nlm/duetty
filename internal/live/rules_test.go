package live

import (
	"testing"

	"github.com/nlm/duetty/internal/stt"
)

func segs(texts ...string) []stt.Segment {
	out := make([]stt.Segment, len(texts))
	for i, t := range texts {
		out[i] = stt.Segment{Start: float64(i), End: float64(i + 1), Text: t}
	}
	return out
}

func TestCommitCount(t *testing.T) {
	cases := []struct {
		texts []string
		want  int
	}{
		{[]string{"Bonjour.", "Je voudrais"}, 1},
		{[]string{"Bonjour.", "Ça va ?", "Et"}, 2},
		{[]string{"We must take", "measures"}, 0},
		{[]string{"« Va chercher Tom. »", "J'ai"}, 1},
		{[]string{"座りませんか？", "駅は"}, 1},
		{[]string{"Fin."}, 0},
		{nil, 0},
		{[]string{"Oui !", "and then", "rest"}, 1},
		{[]string{"Wait…", "(He left.)", "x"}, 2},
	}
	for _, c := range cases {
		if got := CommitCount(segs(c.texts...)); got != c.want {
			t.Errorf("CommitCount(%q) = %d, want %d", c.texts, got, c.want)
		}
	}
}

func TestDue(t *testing.T) {
	cases := []struct {
		newMS, liveMS, intervalMS float64
		want                      bool
	}{
		{1000, 4000, 1000, true},
		{1000, 20000, 1000, false},
		{5000, 20000, 1000, true},
		{900, 1000, 1000, false},
	}
	for _, c := range cases {
		if got := Due(c.newMS, c.liveMS, c.intervalMS); got != c.want {
			t.Errorf("Due(%v, %v, %v) = %v, want %v", c.newMS, c.liveMS, c.intervalMS, got, c.want)
		}
	}
}

func TestJoin(t *testing.T) {
	cases := []struct {
		texts []string
		want  string
	}{
		{[]string{"Bonjour.", "Ça va ?"}, "Bonjour. Ça va ?"},
		{[]string{"座りませんか？", "駅は"}, "座りませんか？駅は"},
		{[]string{"OK.", "駅は"}, "OK. 駅は"},
		{[]string{" a ", "", "b"}, "a b"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := join(segs(c.texts...)); got != c.want {
			t.Errorf("join(%q) = %q, want %q", c.texts, got, c.want)
		}
	}
}
