package live

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nlm/duetty/internal/stt"
)

// bytesPerSec is the size of one second of 48 kHz mono s16le audio.
const bytesPerSec = 96000

// bytesAt returns the byte offset of t seconds into 48 kHz mono s16le audio,
// rounded down to a whole sample.
func bytesAt(t float64) int {
	return int(t*bytesPerSec) &^ 1
}

// msOf returns the duration in milliseconds of n bytes of audio.
func msOf(n int) float64 {
	return float64(n) * 1000 / bytesPerSec
}

// CommitCount returns how many leading segments to commit: up to the last
// segment that ends a sentence and is followed by another segment. The
// newest segment is never committed.
func CommitCount(segs []stt.Segment) int {
	for i := len(segs) - 2; i >= 0; i-- {
		if endsSentence(segs[i].Text) {
			return i + 1
		}
	}
	return 0
}

// endsSentence reports whether s ends in sentence punctuation, ignoring
// trailing spaces, quotes and closing brackets.
func endsSentence(s string) bool {
	s = strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`"'»”’)]}」』）】〉》`, r)
	})
	r, _ := utf8.DecodeLastRuneInString(s)
	return strings.ContainsRune(".?!…。？！", r)
}

// Due reports whether a refresh should be sent: enough new audio since the
// last one, both in absolute terms and relative to the live recording.
func Due(newMS, liveMS, intervalMS float64) bool {
	return newMS >= intervalMS && newMS >= liveMS/4
}

// join concatenates segment texts, separated by a space unless both sides
// of the junction are wide (CJK) characters.
func join(segs []stt.Segment) string {
	var b strings.Builder
	for _, s := range segs {
		t := strings.TrimSpace(s.Text)
		if t == "" {
			continue
		}
		if b.Len() > 0 {
			last, _ := utf8.DecodeLastRuneInString(b.String())
			first, _ := utf8.DecodeRuneInString(t)
			if !wide(last) || !wide(first) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(t)
	}
	return b.String()
}

// wide reports whether r is a Chinese or Japanese character or CJK/full-width
// punctuation, written without spaces between words (Korean uses spaces).
func wide(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK symbols and punctuation
		(r >= 0xFF00 && r <= 0xFFEF) // half-width and full-width forms
}
