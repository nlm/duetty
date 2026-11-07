package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/nlm/poly/internal/convo"
	"github.com/rivo/tview"
)

var pair = [2]string{"fr", "en"}

// plain strips tview tags and unescapes, giving the visible text.
func plain(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '[' {
			if j := strings.IndexByte(s[i:], ']'); j >= 0 {
				if j > 1 && s[i+j-1] == '[' { // escaped "[x[]" -> "[x]"
					b.WriteString(s[i:i+j-1] + "]")
				}
				i += j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func row(seq uint64, st convo.Status, lang, orig, tr string) convo.Row {
	return convo.Row{Seq: seq, At: time.Date(2026, 1, 1, 14, 2, 0, 0, time.Local), Status: st, Lang: lang, Original: orig, Translation: tr}
}

func TestLayoutAlignsRows(t *testing.T) {
	long := "bonjour tout le monde comment allez vous aujourd'hui mon ami je voudrais un cafe"
	rows := []convo.Row{
		row(1, convo.Done, "fr", long, "hello"),
		row(2, convo.Done, "en", "ok", "d'accord"),
	}
	l, r := Layout(rows, pair, 41, false)
	if len(l) != len(r) {
		t.Fatalf("len %d vs %d", len(l), len(r))
	}
	idx := -1
	for i := range r {
		if strings.HasPrefix(plain(r[i]), " » ok") {
			idx = i
		}
	}
	if idx < 4 {
		t.Fatalf("second row too early at %d: %q", idx, l)
	}
	if !strings.HasPrefix(plain(l[idx]), "   d'accord") {
		t.Fatalf("translation not aligned with original: l=%q r=%q", l[idx], r[idx])
	}
	if plain(l[idx-1]) != "" || plain(r[idx-1]) != "" {
		t.Fatalf("expected blank separator")
	}
	if len(l) != idx+1 {
		t.Fatalf("trailing lines: %d", len(l))
	}
}

func TestLayoutMarksSource(t *testing.T) {
	l, r := Layout([]convo.Row{row(1, convo.Done, "en", "hi", "salut")}, pair, 41, false)
	if !strings.HasPrefix(plain(r[0]), " »") || strings.Contains(plain(l[0]), "»") {
		t.Fatalf("l=%q r=%q", l[0], r[0])
	}
	if strings.Contains(r[0], "::b]") || !strings.Contains(r[0], "[gray]hi") {
		t.Fatalf("original should be plain gray: %q", r[0])
	}
	if !strings.HasPrefix(r[0], " [gray]»[-::-] ") {
		t.Fatalf("marker should share the original's gray: %q", r[0])
	}
	if !strings.Contains(l[0], "[-::-]salut") {
		t.Fatalf("translation should be plain default color: %q", l[0])
	}
	l, r = Layout([]convo.Row{row(1, convo.Done, "fr", "salut", "hi")}, pair, 41, false)
	if !strings.HasPrefix(plain(l[0]), " »") || strings.Contains(plain(r[0]), "»") {
		t.Fatalf("fr: l=%q r=%q", l[0], r[0])
	}
}

func TestLayoutCJKWidth(t *testing.T) {
	jp := strings.Repeat("あ", 30)
	l, r := Layout([]convo.Row{row(1, convo.Done, "ja", jp, "short")}, [2]string{"ja", "en"}, 41, false)
	if len(l) != len(r) || len(l) < 3 {
		t.Fatalf("len %d %d", len(l), len(r))
	}
	for _, ln := range append(append([]string{}, l...), r...) {
		if w := runewidth.StringWidth(plain(ln)); w > 20 {
			t.Fatalf("line %q is %d cells", ln, w)
		}
	}
	if !strings.HasPrefix(plain(l[0]), " »") || !strings.HasPrefix(plain(r[0]), "   short") {
		t.Fatalf("rows not aligned: %q %q", l[0], r[0])
	}
	if !strings.HasPrefix(plain(l[1]), "   ") {
		t.Fatalf("no indent: %q", l[1])
	}
}

func TestLayoutNarrow(t *testing.T) {
	text := "日本語のテストです hello wonderful 世界"
	for w := 0; w <= 60; w++ {
		for _, ts := range []bool{false, true} {
			l, r := Layout([]convo.Row{row(1, convo.Done, "fr", text, "x")}, pair, w, ts)
			if len(l) != len(r) {
				t.Fatalf("w=%d misaligned", w)
			}
			cw := (w - 1) / 2
			// A column narrower than "» " plus one wide rune (4 cells)
			// physically cannot hold a CJK glyph; otherwise every line fits.
			limit := cw
			if limit < 4 {
				limit = 4
			}
			for _, ln := range append(append([]string{}, l...), r...) {
				if got := runewidth.StringWidth(plain(ln)); got > limit {
					t.Fatalf("w=%d ts=%v line %q is %d cells (limit %d)", w, ts, ln, got, limit)
				}
			}
		}
	}
}

func TestLayoutPendingTranslation(t *testing.T) {
	l, r := Layout([]convo.Row{row(1, convo.Original, "fr", "salut", "")}, pair, 41, false)
	if !strings.Contains(plain(r[0]), "…") || !strings.Contains(r[0], "[gray::i]") {
		t.Fatalf("%q %q", l, r)
	}
}

func TestLayoutCancelledAndFailed(t *testing.T) {
	_, r := Layout([]convo.Row{row(1, convo.Cancelled, "fr", "salut", "")}, pair, 41, false)
	if !strings.Contains(plain(r[0]), "cancelled") {
		t.Fatalf("%q", r)
	}
	if l, _ := Layout([]convo.Row{row(1, convo.Cancelled, "fr", "salut", "")}, pair, 41, false); !strings.Contains(l[0], "[gray::s]salut") {
		t.Fatalf("cancelled original not struck through: %q", l)
	}
	if false {
		t.Fatalf("%q", r)
	}
	f := row(1, convo.Failed, "fr", "salut", "")
	f.Err = "translation failed"
	_, r = Layout([]convo.Row{f}, pair, 41, false)
	if !strings.Contains(strings.Join(mapPlain(r), " "), "translation failed") || !strings.Contains(r[0], "[red]") {
		t.Fatalf("%q", r)
	}
}

func TestLayoutTimestamps(t *testing.T) {
	rows := []convo.Row{row(1, convo.Done, "fr", "salut", "hi")}
	l, r := Layout(rows, pair, 41, false)
	if strings.Contains(plain(l[0]), "14:02") || strings.Contains(plain(r[0]), "14:02") {
		t.Fatal("timestamp shown when off")
	}
	l, r = Layout(rows, pair, 41, true)
	if !strings.HasPrefix(plain(l[0]), " 14:02 ") || !strings.HasPrefix(plain(r[0]), " 14:02 ") {
		t.Fatalf("%q %q", l[0], r[0])
	}
}

func TestLayoutEscapesTags(t *testing.T) {
	l, _ := Layout([]convo.Row{row(1, convo.Done, "fr", "[red]hi", "x")}, pair, 41, false)
	if !strings.Contains(plain(l[0]), "[red]hi") {
		t.Fatalf("%q", l[0])
	}
	if got := tview.TaggedStringWidth(l[0]); got != runewidth.StringWidth(" » [red]hi") {
		t.Fatalf("width %d for %q", got, l[0])
	}
}

func mapPlain(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = strings.TrimSpace(plain(l))
	}
	return out
}

func TestLayoutMergesSameLanguage(t *testing.T) {
	rows := []convo.Row{
		row(1, convo.Done, "fr", "Bonjour.", "Hello."),
		row(2, convo.Done, "fr", "Ça va ?", "How are you?"),
		row(3, convo.Original, "fr", "Et toi ?", ""),
		row(4, convo.Done, "en", "Fine.", "Bien."),
		row(5, convo.Done, "fr", "Super.", "Great."),
	}
	l, r := Layout(rows, pair, 81, false)
	if len(l) != len(r) {
		t.Fatalf("len %d vs %d", len(l), len(r))
	}
	pl, pr := mapPlain(l), mapPlain(r)
	want := []string{"» Bonjour. Ça va ? Et toi ?", "", "Bien.", "", "» Super."}
	wantR := []string{"Hello. How are you? …", "", "» Fine.", "", "Great."}
	for i := range want {
		if pl[i] != want[i] || pr[i] != wantR[i] {
			t.Fatalf("line %d: %q | %q\nleft=%q\nright=%q", i, pl[i], pr[i], pl, pr)
		}
	}
	if !strings.Contains(r[0], "[-::-]How are you? [-::-][gray::i]…") {
		t.Fatalf("styles not kept per sentence: %q", r[0])
	}
}

func TestLayoutMergesCJKWithoutSpace(t *testing.T) {
	rows := []convo.Row{
		row(1, convo.Done, "ja", "こんにちは。", "Hello."),
		row(2, convo.Done, "ja", "元気？", "How are you?"),
	}
	l, _ := Layout(rows, [2]string{"ja", "en"}, 81, false)
	if got := strings.TrimSpace(plain(l[0])); got != "» こんにちは。元気？" {
		t.Fatalf("%q", got)
	}
}

func TestLayoutStyleSplitAcrossWrap(t *testing.T) {
	rows := []convo.Row{
		row(1, convo.Done, "fr", "a", "one two three"),
		row(2, convo.Cancelled, "fr", "b", ""),
	}
	_, r := Layout(rows, pair, 31, false) // 15-cell columns, 11 for text
	joined := strings.Join(r, "\n")
	if !strings.Contains(joined, "[-::-]three") || !strings.Contains(joined, "[gray::d](cancelled)") {
		t.Fatalf("%q", r)
	}
}

func TestLayoutLiveRow(t *testing.T) {
	live := row(2, convo.Live, "fr", "Je voudrais", "I would like")
	live.Stale = true
	l, r := Layout([]convo.Row{row(1, convo.Done, "fr", "Bonjour.", "Hello."), live}, pair, 100, false)
	if got := strings.TrimSpace(plain(l[0])); got != "» Bonjour. Je voudrais" {
		t.Fatalf("left = %q", got)
	}
	if !strings.Contains(l[0], "[gray::i]Je voudrais") {
		t.Fatalf("live source style: %q", l[0])
	}
	if !strings.Contains(r[0], "[gray::i]I would like") || !strings.Contains(plain(r[0]), "I would like …") {
		t.Fatalf("live translation: %q", r[0])
	}
}

func TestLayoutLiveNoTranslationYet(t *testing.T) {
	l, _ := Layout([]convo.Row{row(1, convo.Live, "en", "Hello th", "")}, pair, 100, false)
	if !strings.Contains(l[0], "[gray::i]…") {
		t.Fatalf("left = %q", l[0])
	}
}
