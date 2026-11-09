# duetty evaluation

`duetty-eval` runs clips through the same path as the live app (Silero probs,
trim, min-speech check, then `stt.Resolver`) against the real API, once per
language pair. Auto-detect transcriptions are cached per (clip, language) within a run,
since the first pass does not depend on the pair. Transcripts are printed to
stdout only; nothing is stored. Audio is never committed (`eval/clips/`,
`eval/words/`, `eval/noise/` are gitignored).

```
go build -o duetty-eval ./cmd/duetty-eval          # models/ and third_party/ next to the binary
scripts/fetch-tatoeba-clips.sh fr,en,es,ja 10 eval/clips
WORD_MIN=1 WORD_MAX=2 scripts/fetch-tatoeba-clips.sh fr,en,es,ja 12 eval/words
DUETTY_API_KEY=... ./duetty-eval -config my.toml -clips eval/clips/meta.json \
    -words eval/words/meta.json -noise eval/noise -detail
```

`-floor` / `-maxcomp` override the noise thresholds; `-detail` prints each
Whisper pass's avg_logprob and compression ratio for words and noise;
`vad_threshold` / `vad_min_speech_ms` come from the config file.
Tatoeba audio licenses vary per recording (mostly CC BY 2.0 FR / CC BY-NC-ND), so only
the fetch script is in the repo.

## Language pick (whisper-large-v3 via Scaleway, 10 random Tatoeba sentences per language, >= 3 words)

| pair  | correct |
|-------|---------|
| fr-en | 20/20 |
| es-en | 20/20 |
| ja-en | 20/20 |
| es-fr | 20/20 |
| ja-fr | 20/20 |
| es-ja | 20/20 |

## Noise calibration

Words: 57 short real utterances (1-2 words: fr/en/es/ja, incl. Yes./No./Sí./Vale./Gracias.).
Noise: 25 clips. 14 synthetic (ffmpeg: white/pink/brown noise 0.4/1.0/1.5 s,
50 Hz hum (+ harmonic), 1 Hz click impulses, two keyboard-like click trains)
and 11 real recordings from Wikimedia Commons (trimmed to <= 4 s):

| clip | source (commons.wikimedia.org/wiki/File:...) | license |
|------|------|------|
| real-cough-S1/S3/S4/S9 | The-description-of-cough-sounds-by-healthcare-professionals-1745-9974-2-1-S1/S3/S4/S9.ogg | CC BY 2.0 |
| real-door-slam | Slam closet door (Gravity Sound).wav | CC BY 4.0 |
| real-footstep-gravel | Footstep on Gravel (Gravity Sound).mp3 | CC BY 4.0 |
| real-laughter | 72844 lonemonk approx-800-laughter-and-clapter-1.wav | CC BY 3.0 |
| real-keyboard | Keyboard noise.ogg | public domain |
| real-typing-modelF | Typing - Model F 122 1984.ogg | CC0 |
| real-sneeze | Sneeze.ogg | CC0 |
| real-sneezing | Sneezing.ogg | public domain |

Results with the shipped defaults (silero 0.6, min speech 250 ms, logprob floor -1.0, max compression 2.4):

| stage | result (all six pairs identical) |
|-------|--------|
| short words kept | 26/26, 29/29, 26/26, 27/27, 24/24, 27/27 (fr-en, es-en, ja-en, es-fr, ja-fr, es-ja) |
| noise dropped before Whisper (Silero/min-speech/trim) | 25/25 |
| noise reaching Whisper -> ErrNoise / row | 0 / 0 |

Silero alone removes every noise clip, so the Whisper-level thresholds were
stress-tested with Silero loosened (`vad_threshold = 0.05`,
`vad_min_speech_ms = 0`), which lets 10 of 25 clips through (fr-en pair):

| clip | avg_logprob | compression | Whisper text | verdict at -1.0 |
|------|------|------|------|------|
| real-cough-S3 / S4 / S9 | -0.81 / -0.62 / -0.64 | 0.50 / 0.50 / 0.58 | "*cough*", "*coughing*" | row |
| real-laughter | -1.66 | 0.20 | "-" | dropped |
| real-sneeze / sneezing | -1.17 / -1.67 | 0.53 / 0.47 | "*laughs*", "*Sigh*" | dropped |
| syn-hum50h | -0.83 | 0.33 | "you" | row |
| syn-brown-1.0 / keys1 / keys2 | -1.30 / -1.58 / -1.00 | 0.33 | "you" | dropped |

Real words, all Whisper passes: avg_logprob median -0.28, 10th percentile
-0.55; the lowest are one badly recorded Tatoeba clip (fr-473963: -0.89 / -0.94 /
-1.05 / -0.61 depending on forced language) and then -0.67, -0.55. Compression
ratio is 0.4-0.6 for words and for every noise survivor, so it does not
separate them at all (it only catches repetition loops, which none of these
produced).

Conclusion: the defaults (`noise_min_logprob = -1.0`, `noise_max_compression = 2.4`)
are kept. Moving the floor to -0.85 would only gain the "you" hum clip at a
cost of the one bad real word clip; the coughs ("*cough*", -0.62..-0.81) overlap the
range of real words, so no logprob threshold can drop them without dropping
real words. Silero + min-speech is the real noise filter (25/25 dropped);
the Whisper-level floor is a backstop for low-confidence hallucinations
(laughter, sneezes, noise bursts: 6 of 10 stress survivors).

## Denoise (`-denoise`, GTCRN)

`duetty-eval -denoise` runs every clip (sentences, words, noise) through a fresh
`internal/denoise` instance in 2048-byte chunks, as the live mic path does,
before Silero. Same clips and defaults as above, run back to back:

| stage | denoise off | denoise on |
|-------|-------------|------------|
| language pick (6 pairs) | 20/20 each | 20/20 each |
| short words kept | 26/26, 29/29, 26/26, 27/27, 24/24, 27/27 | 26/26, 28/29, 26/26, 26/27, 24/24, 26/27 |
| noise dropped before Whisper | 25/25 each | 24/25 each |
| noise reaching Whisper -> ErrNoise / row | 0 / 0 | fr-en, es-en, ja-en, es-fr: 0 / 1; ja-fr, es-ja: 1 / 0 |

Language pick does not drop. The differences are both at Silero's edge: the
short "Sí." (es-35900) falls to 224 ms voiced (< 250 ms min speech) once
denoised, and `real-sneezing` (a vocal sound, not stationary noise) passes
Silero after its background is removed. These clips are clean studio-ish
recordings, so denoise can only lose here; it is meant for noisy rooms
(cafés, trains) and stays off by default.

## Context, correction and hints

Measured 2026-11-09 against Scaleway (whisper-large-v3, mistral-small-3.2-24b-instruct-2506),
same clips, words and noise as above, runs back to back. The fresh baseline (no
`-context`) matched the tables above exactly (20/20 per pair, words 26/26, 29/29,
26/26, 27/27, 24/24, 27/27, noise 25/25 dropped before Whisper). Offline
(speaches + Ollama) was not measured: not available on the measuring machine.

`-correct` (102 sentence clips, all pairs; heard text translated with and
without correction). "Before the change check" is the first measurement; "with
the change check" re-ran it after duetty started checking each change of a
correction (sound-alike replacements and small missing words only; a dropped
change keeps the heard words and the sentence is retranslated):

| measure | no context, before the change check | no context, with the change check | example.md, before the change check | example.md, with the change check |
|---------|------|------|------|------|
| rephrased (clips heard correctly, REPHRASE) | 0/102 | 0/102 | 11/102 | 1/102 |
| discarded corrections (DISCARDED) | 0/102 | 0/102 | 0/102 | 17/102 |
| errors | 0 | 0 | 0 | 0 |
| median latency plain / correct | 175 ms / 466 ms (+291 ms) | 111 ms / 294 ms (+183 ms) | 132 ms / 304 ms (+172 ms) | 115 ms / 329 ms (+214 ms) |

The context runs use `-context internal/ctxfile/testdata/example.md`.

Before the change check, the context file made the model swap a heard name for
a name from the file: "Tom" became "Dan" or "Théo" in 6 distinct clips
(en-1082811, en-120146, en-213632, en-547924, fr-1093576, fr-77020; a clip can
count in several pairs), and fr-1098032 "J'ai ouvert le capot." became
"J'ai ouvert le bucket." The whole-sentence similarity check did not discard
them (short sentences, one word changed).

With the change check (measured 2026-11-09), all of those swaps are dropped,
and the heard text is kept and retranslated. This run includes both review
fixes: names and context terms are checked inside a replaced span, and a
replaced span with the same number of words is judged word by word. The eval
reports a clip whose every change was dropped as DISCARDED, because the
correction comes back empty when it equals the heard text. So the 17 DISCARDED
are the intended outcome: 8 distinct clips (the 7 above plus en-1148324 "Tell
Tom he was wrong."), all shown as heard. The real fix `heard="J'avais de me
reposer." corrected="J'avais besoin de me reposer."` is still kept in both
runs. `sameWords` in the eval now ignores punctuation, so an added comma in
Japanese (no spaces) no longer counts as a changed word. An earlier run counted
ja-1053503 as a REPHRASE for that reason alone.

Not a full pass: one REPHRASE remains with example.md:
`es-609293.wav: heard="Piense en su familia." corrected="Pense en su familia."  REPHRASE`.
"Piense" -> "Pense" is a one-letter change (similarity 0.83), so it passes as a
sound-alike fix. `minSpanSimilarity` (0.5) was not tuned: dropping this would
need a threshold above 0.83, which would also drop one-letter mishearing fixes.
In an earlier run the model returned "Pense à sa famille." for this clip. The
word-by-word check now drops that ("en" -> "à").

Latency stays under the 300 ms threshold. A dropped change costs one more
translation-only request for that sentence.

Whisper hints (`-context internal/ctxfile/testdata/example.md`, hand-written hint
section; and a copy without the hint section, so hints were generated):

| stage | no context | example.md hint | generated hint |
|-------|-----------|-----------------|----------------|
| language pick (6 pairs) | 20/20 each | 20/20 each | 20/20 each |
| short words kept (fr-en, es-en, ja-en, es-fr, ja-fr, es-ja) | 26/26, 29/29, 26/26, 27/27, 24/24, 27/27 | 25/26, 29/29, 26/26, 26/27, 23/24, 27/27 | 25/26, 29/29, 26/26, 26/27, 24/24, 27/27 |
| noise dropped before Whisper | 25/25 each | 25/25 each | 25/25 each |
| noise reaching Whisper -> ErrNoise / row | 0 / 0 | 0 / 0 | 0 / 0 |

The only regression is the one badly recorded word clip (fr-473963 "Tom peint.",
see the noise calibration above), dropped as noise in 3 of 6 pairs with the
hint and in 2 of 6 pairs with the generated one; without a hint it was kept in
all 6 pairs and in two further back-to-back re-runs (fr-en, es-fr, ja-fr).
Re-running the example.md hint on the three fr pairs gave fr-en 25/26 (fr-473963
dropped), es-fr 27/27 and ja-fr 24/24. So the drop of fr-473963 was not
reproducible every time, and the verdict stands under the rule.
`-detail` (fr-en) shows the hint lowers the forced-language passes
(en: -0.97 -> -1.43, "Dun-dun." -> "Thumb up!"; fr: -1.19 -> -1.57, "ton pain." ->
"Tum pan!"), under the -1.0 floor. The output is not a copy of the hint, so the
echo filter from the plan (drop >= 5-word substrings of the hint) would not
apply and was not added. Noise did not regress.

Verdict: the words check fails ("equal to the table"), so `whisper_hint` ships
off by default (`-whisper-hint` / `whisper_hint = true` to enable). The context
file still feeds the translator and correction.

Prompt caching: `cached_tokens` was 0 on every translation response (about 470-500
prompt tokens each, 20 calls on fr-en with the context file). No caching observed
on Scaleway for this size of prompt; information only.
