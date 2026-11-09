// Package ctxfile reads the optional Markdown context file that duetty
// uses for translation and Whisper hints. It only reads; nothing is written.
package ctxfile

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// WarnTokens is the approximate size above which a context file is
// considered large enough to warn about.
const WarnTokens = 2000

// File is a parsed context file.
type File struct {
	// Body is the whole file minus the "Whisper hint" section, trimmed.
	Body string
	// Hints maps a lowercase language code to its Whisper hint text.
	// It is nil when the file has no hint section.
	Hints map[string]string
}

// Load reads and parses the context file at path.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("context file: %w", err)
	}
	return Parse(string(b)), nil
}

// ApproxTokens estimates the token count of the body (runes / 4).
func (f File) ApproxTokens() int {
	return utf8.RuneCountInString(f.Body) / 4
}

// heading returns the level and text of a Markdown heading line (#-######).
func heading(line string) (int, string, bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || (line[n] != ' ' && line[n] != '\t') {
		return 0, "", false
	}
	return n, strings.TrimSpace(line[n:]), true
}

// Parse parses context file source text.
func Parse(src string) File {
	src = strings.TrimPrefix(src, "\ufeff")
	src = strings.ReplaceAll(src, "\r\n", "\n")

	var body []string
	var hints map[string]string
	var cur strings.Builder
	curLang := ""
	level := 0 // level of the hint heading while inside the section

	flush := func() {
		if curLang != "" {
			hints[curLang] = strings.TrimSpace(cur.String())
		}
		cur.Reset()
		curLang = ""
	}

	for _, line := range strings.Split(src, "\n") {
		lvl, text, ok := heading(line)
		if level == 0 {
			if ok && lvl <= 3 && strings.EqualFold(text, "whisper hint") {
				level = lvl
				if hints == nil {
					hints = map[string]string{}
				}
				continue
			}
			body = append(body, line)
			continue
		}
		if ok && lvl <= level {
			flush()
			level = 0
			if lvl <= 3 && strings.EqualFold(text, "whisper hint") {
				level = lvl
				continue
			}
			body = append(body, line)
			continue
		}
		if ok && lvl == level+1 {
			flush()
			curLang = strings.ToLower(text)
			continue
		}
		if curLang != "" {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	flush()
	return File{Body: strings.TrimSpace(strings.Join(body, "\n")), Hints: hints}
}
