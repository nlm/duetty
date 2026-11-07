# poly — two-language live transcript for face-to-face conversation

Date: 2026-11-07
Status: approved by user 2026-11-07 (es/ja verified)

## Intent and why

Two people who don't share a language sit at one laptop, sharing one
microphone and one screen. Anything either of them says (or types) appears
almost immediately in **two side-by-side panes**, one per language: the
original in the speaker's language, and its translation in the other.

Why this and not existing interpreters: turn-based tools that speak the
translation aloud make everyone wait, the other person doesn't understand
they must wait and talks over it, you can't tell who said what, and audio is
easy to mishear. Reading is faster than listening. Seeing both versions
also gives the **speaker** feedback: they read their own sentence as it was
transcribed and know whether they were understood.

There is no chatbot. The app never answers, it only transcribes and
translates.

## Goals

- Pick two languages at startup (config file or flag). Every utterance is
  shown in both.
- Hands-free: always-listening mic, voice activity detection and smart
  end-of-turn detection, no push-to-talk.
- Low latency: the finished sentence shows up in both panes about 1–1.5 s
  after the speaker stops.
- Typing is an alternative to speaking.
- Mistakes are cheap: if a line is wrong, say it again. The wrong line stays
  visible.

- **Any pair of Whisper-supported languages** must work. French/English is
  the first priority: it's tested and tuned first, and the default pair.

## Non-goals (for now)

- **No speaker audio / TTS** at all.
- **No live preview** while someone is speaking (partial transcripts).
  Explicitly deferred: "later". The design should not prevent adding it.
- No speaker identification (voice recognition). The language tells the
  two people apart well enough.
- No persistence: no transcript file, no history, no resume.
- No support for more than two languages in one session.

## Anti-goals (would make it a failure even if it "works")

- Making people wait for the screen or forcing a turn order. People speak
  whenever they want.
- The screen changing without the user seeing why (rows disappearing or
  silently rewritten).
- Conversation text written to disk anywhere, including logs.

## Constraints (decided by the user)

- Go, terminal UI with **tview/tcell** (same kit as talktome).
- Built from talktome's mechanisms (`~/llmchat/talktome`, module
  `github.com/nlm/talktome`): mic capture, **Silero VAD**, **Smart Turn v3**.
- Speech-to-text via an **OpenAI-compatible transcription API**, Whisper
  (`stt_backend = "openai"`-style). The user's endpoint is Scaleway
  Generative APIs: `https://api.scaleway.ai/<project_id>/v1`, model
  `whisper-large-v3`. Credentials for testing are in
  `~/llm-benchmark-creds.yaml` (`secret_key` is the bearer token,
  `default_project_id` goes in the URL). Never print or commit the key.
- One laptop, one shared microphone, one screen.
- Timestamps not shown by default. `-timestamps` turns them on.
- Escape cancels any pending (in-flight) transcription/translation.
- Everything disappears on quit.

## Findings that shape the design (verified against the API, 2026-11-07)

1. **Why talktome "translates":** talktome sends `language=<configured>`
   on every transcription (`internal/openai/openai.go:645`). When the
   audio is in another language, Whisper decodes with the forced
   language token and outputs a rough translation instead of a
   transcript. This is unreliable and must not be used as the
   translation mechanism.
2. **Language choice — auto-detection wins (tested on 49 clips).** Real
   human recordings from Tatoeba (10 each of es, ja, fr, en, mostly short
   3–6 word sentences) plus 9 longer local clips:
   - Whisper auto-detection (no `language`, `verbose_json` → `language`):
     **48/49 correct**. The one miss was a synthetic TTS voice
     (`french-test.wav`, detected `en`, garbled).
   - Forced-language pick (transcribe forced to each language, keep the
     higher token-weighted `avg_logprob`): only **85–95% per pair**
     (fr/en 17/20, es/en 18/20, ja/en 17/20, es/fr 17/20, ja/fr 19/20,
     es/ja 19/20). Whisper often translates fluently and *confidently*
     into the forced wrong language ("Je vais voir s'il est là." → forced
     en "I'll see if he's here.", higher score than forced fr), and the
     two scores on real speech are often within 0.01–0.05 of each other.
     **So the logprob comparison must not be the primary method.**
   - In several forced-pass misses, Whisper ignored the wrong forced
     language and echoed the original ("Nous en parlons constamment." from
     forced en). A text language check catches those.
   - `no_speech_prob` comes back `null` on this endpoint, `words` is
     `null`; `avg_logprob` and `compression_ratio` per segment are present.
3. (Earlier 4-clip result where forced-logprob looked perfect was too small
   a sample; superseded by finding 2.)
4. **No audio input streaming:** no realtime endpoint (`/realtime` → 404).
   `stream=true` on `/audio/transcriptions` streams *output* text deltas
   (SSE `transcript.text.delta`), but the full audio must be sent first.
   Not useful now, since transcription is already ~0.5–0.8 s.
5. **Translation latency** (one short sentence, temperature 0):
   `mistral-small-3.2-24b-instruct-2506` 0.36 s, `qwen3-235b-a22b-instruct-2507`
   0.8 s, `gemma-4-26b-a4b-it` 3.7 s. All translated correctly.

## How it works

### Pipeline per utterance

1. **Capture:** mic → Silero VAD → Smart Turn decides the end of the
   utterance → one PCM buffer (talktome's `internal/vad` Listener does this
   and delivers whole utterances on a channel).
2. **No row yet.** The status bar shows "working (n)" (transcriptions and translations in flight) instead. A row is
   only added once a transcript exists, so noise that turns out to be
   nothing never makes a row appear and then vanish. Rows are still placed
   in speaking order (by capture time), not completion order.
3. **Transcribe once, auto-detect:** the WAV goes to `/audio/transcriptions`
   with no `language`, `verbose_json`, `temperature=0`. If the returned
   `language` is one of the two configured languages, that is the row's
   source language and the text is the original. Sanity check: run a
   local offline text language identifier (Claude's call:
   `github.com/pemistahl/lingua-go`, pure Go, restricted to the two
   configured languages) on the text. If it confidently says the *other*
   pair language, re-transcribe forced to that language.
   **Fallback when the detected language is outside the pair** (e.g. `pt`
   when the pair is es/en, or a mumble): transcribe twice in parallel,
   forced to A and to B. Pick with the text identifier first (a forced
   pass whose output is clearly in the *other* pair language is an echo of
   the original, so the original language is that one); only if the text
   check is inconclusive, use the higher token-weighted `avg_logprob`.
   This fallback costs one extra round trip (~0.6 s) and should be rare.
   On API failure (after the fallback, if any), add a row marked as an error.
   **Noise handling** (decided with the user: stop noise before Whisper
   rather than filter Whisper's output afterwards):
   - **Silero VAD only, no energy detector.** Speech threshold configurable
     (`silero_vad_threshold`, default 0.6, slightly stricter than talktome's
     0.5 to reject non-voice noise). Minimum speech duration configurable
     (`vad_min_speech_ms`, default 250 ms, low enough to keep "oui"/"yes").
   - **Trim before sending:** use Silero's per-frame speech decisions to cut
     leading/trailing non-speech (keep ~150 ms margin) so Whisper gets only
     the voiced part. Silent padding is the main cause of Whisper
     hallucinations ("Merci.", "Thank you.").
   - **Confidence floor:** drop the result when its token-weighted
     `avg_logprob` is below a floor or `compression_ratio` is abnormally
     high (repetition loops). Do NOT use the gap between forced passes as a
     noise signal: real speech often has tiny gaps (finding 2). The builder
     calibrates thresholds on recorded clips (coughs, chair noise, laughter,
     short real words like "oui"/"yes"/"はい").
   - A stock-hallucination phrase list is a last resort, only if the above
     proves insufficient on real recordings.
   - A dropped utterance produces no row and only clears its status-bar
     counter.
   - **Optional local denoising** (`denoise = false` / `-denoise`, off by
     default): a local ONNX noise-suppression model (DeepFilterNet3 or
     similar, run in-process via onnxruntime like Silero) cleans mic audio
     before VAD and Whisper. It's meant for cafés, trains and open offices.
     The builder picks the concrete model (must be ONNX, local, real-time on
     CPU, ideally 48 kHz like the mic) and adds a setup script like
     talktome's `scripts/setup-*.sh`. If the model is missing, warn and run
     without it.
4. **Add the row and show the original** in its language's pane (marked ●, brighter). The
   other pane shows "…translating".
5. **Translate:** one `/chat/completions` call, temperature 0, system prompt
   "translate from A to B, output only the translation". It includes the last
   few rows (original + translation) as context, so short replies and
   pronouns translate correctly. The result fills the other pane.

Several utterances can be in flight at once. Rows keep speaking order no
matter which finishes first.

### Typed input

The input box at the bottom: Enter sends. The source language is decided
by the same local text language identifier (lingua-go restricted to the
two languages), with no API call. Then the same steps 4–5 run. Typed rows look the same as
spoken ones.

### Cancel

Escape cancels every pending transcription and translation (context
cancellation). Affected rows stay visible, greyed, marked "cancelled". The
mic keeps listening.

### Screen

```
┌─ Français ───────────────────────────┬─ English ────────────────────────────┐
│ ● Bonjour, vous êtes arrivé quand ?  │   Hello, when did you arrive?        │
│                                      │                                      │
│   Je suis arrivé hier soir, en       │ ● I arrived yesterday evening, by    │
│   train.                             │   train.                             │
│                                      │                                      │
│ ● On peut se voir demain ?           │   …translating                       │
├──────────────────────────────────────┴──────────────────────────────────────┤
│ ▮▮▮▮▮▯▯▯  listening                                  fr ⇄ en   mistral-small │
├─────────────────────────────────────────────────────────────────────────────┤
│ > _                                                                         │
└─────────────────────────────────────────────────────────────────────────────┘
```

- Left pane = first configured language, right = second. Header shows the
  language name.
- Rows are **aligned**: a row takes the height of its taller side, so both
  versions of a sentence start on the same line.
- ● + bright = what was actually said or typed. Plain/softer color =
  translation.
- Status bar: mic level meter, state (listening / hearing / muted),
  number of in-flight requests if any, language pair, chat model.
- Both panes scroll together (PgUp/PgDn, mouse wheel, Shift-drag to select
  text, as in talktome). New rows auto-scroll unless the user scrolled up.
- Errors (API down, etc.) show as a short line in the row or status bar,
  never a crash.

### Keys

- Enter: send typed text
- Esc: cancel pending
- Ctrl+P: mute/unmute mic (as in talktome)
- PgUp/PgDn, wheel: scroll
- Ctrl+C: quit

### Configuration

talktome style: `config.toml` + command-line flags that override it.

- `languages = ["fr", "en"]` / `-langs fr,en` (ISO-639-1; exactly two,
  distinct; must be Whisper-supported)
- endpoint base URL, API key (env `OPENAI_API_KEY` like talktome, or config)
- `stt_model` (default `whisper-large-v3`), `translate_model` (default
  `mistral-small-3.2-24b-instruct-2506`)
- `timestamps = false` / `-timestamps`
- `silero_vad_threshold` (0.6), `vad_min_speech_ms` (250), `denoise` (false),
  `denoise_model` path
- VAD/Silero/Smart Turn keys with talktome's names and defaults (model paths,
  thresholds, silence ms, min speech ms, device selection). Unlike talktome,
  **Silero and Smart Turn are on by default**. If a model file is missing,
  fall back with a warning, as talktome does. Models exist at
  `~/llmchat/talktome/models/silero_vad.onnx` and
  `~/llmchat/talktome/models/smart-turn/smart-turn-v3.2-cpu.onnx`.
- `translate_context = 4` / `-translate-context N`: number of previous
  sentences (original + translation) sent as context with each translation
  request. Default 4, user-confirmed. 0 disables context.

### Privacy

No conversation text is written to disk. Any debug log holds only timings,
states and errors, never transcripts or translations.

## Decisions left to the builder / Claude's calls

These are Claude's own calls, not the user's:

- **Reuse by copying** the needed talktome packages (audioio, vad,
  silerovad, smartturn, audioutil, audiolevel, onnxenv, the transcription
  part of openai, relevant tui bits) into `/root/poly` rather than
  importing them. talktome is a large module with internal packages, and
  copying keeps poly standalone. Trim what isn't needed.
- Project location `/root/poly`, binary name `poly`.
- Translation model default `mistral-small-3.2` (fastest in the tests).
- Text language detection with lingua-go (also used for typed input).
- Testing: an offline evaluation command (like talktome's `eval`) runs
  recorded clips per language pair through the pick logic. FR/EN clips
  first, plus es and ja (the Tatoeba clips used in finding 2 can seed the
  test set; fetch them by a script, don't commit third-party audio unless
  the license allows it — Tatoeba audio licenses vary per recording). Pairs never
  evaluated still work. No startup warning.
- Cancelled rows stay greyed with "cancelled". (An utterance cancelled
  before its transcript exists has no row and just disappears from the
  status-bar counter.)
- No placeholder row before transcription.
- Defaults: Silero threshold 0.6, min speech 250 ms, trim margin 150 ms;
  confidence-floor thresholds calibrated on clips.
- Auto-detect first, forced-pair fallback (chosen from the 49-clip test).
- Choice of denoise model.
- Silero + Smart Turn on by default.
- Exact colors, help line, package layout, test strategy.
