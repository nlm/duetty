# poly evaluation

`poly-eval` runs clips through the same path as the live app (Silero probs,
trim, min-speech check, then `stt.Resolver`) against the real API, once per
language pair. Auto-detect transcriptions are cached per (clip, language) within a run,
since the first pass does not depend on the pair. Transcripts are printed to
stdout only; nothing is stored. Audio is never committed (`eval/clips/`,
`eval/words/`, `eval/noise/` are gitignored).

```
go build -o poly-eval ./cmd/poly-eval          # models/ and third_party/ next to the binary
scripts/fetch-tatoeba-clips.sh fr,en,es,ja 10 eval/clips
WORD_MIN=1 WORD_MAX=2 scripts/fetch-tatoeba-clips.sh fr,en,es,ja 12 eval/words
OPENAI_API_KEY=... ./poly-eval -config my.toml -clips eval/clips/meta.json \
    -words eval/words/meta.json -noise eval/noise -detail
```

`-floor` / `-maxcomp` override the noise thresholds; `-detail` prints each
Whisper pass's avg_logprob and compression ratio for words and noise;
`silero_vad_threshold` / `vad_min_speech_ms` come from the config file.
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
stress-tested with Silero loosened (`silero_vad_threshold = 0.05`,
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

Conclusion: the defaults (`noise_logprob_floor = -1.0`, `noise_max_compression = 2.4`)
are kept. Moving the floor to -0.85 would only gain the "you" hum clip at a
cost of the one bad real word clip; the coughs ("*cough*", -0.62..-0.81) overlap the
range of real words, so no logprob threshold can drop them without dropping
real words. Silero + min-speech is the real noise filter (25/25 dropped);
the Whisper-level floor is a backstop for low-confidence hallucinations
(laughter, sneezes, noise bursts: 6 of 10 stress survivors).

## Denoise (`-denoise`, GTCRN)

`poly-eval -denoise` runs every clip (sentences, words, noise) through a fresh
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
