package translate

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// maxHintWords bounds each generated Whisper hint.
const maxHintWords = 150

// WhisperHints asks the model for one short Whisper prompt per language of
// the pair, built from the context. Only the two pair codes are kept, each
// truncated to 150 words; empty values are dropped. It fails when neither
// language yields a hint.
func (c *Client) WhisperHints(ctx context.Context, contextBody string, langs [2]string) (map[string]string, error) {
	system := fmt.Sprintf("You write speech recognition hints. From the context below, write for each language "+
		"one or two natural sentences in that language that use the context's hardest words first "+
		"(names, brands, unusual spellings), at most %d words. "+
		`Reply with JSON only: {"%s": "...", "%s": "..."}.`,
		maxHintWords, langs[0], langs[1])
	content, err := c.complete(ctx, []message{
		{"system", system},
		{"user", contextBody},
	})
	if err != nil {
		return nil, err
	}
	var raw map[string]string
	if err := ExtractJSONObject(content, &raw); err != nil {
		return nil, fmt.Errorf("whisper hints: %w", err)
	}
	hints := make(map[string]string, 2)
	for _, l := range langs {
		words := strings.Fields(raw[l])
		if len(words) > maxHintWords {
			words = words[:maxHintWords]
		}
		if len(words) > 0 {
			hints[l] = strings.Join(words, " ")
		}
	}
	if len(hints) == 0 {
		return nil, errors.New("whisper hints: empty reply")
	}
	return hints, nil
}
