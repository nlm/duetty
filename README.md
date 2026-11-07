# poly

A terminal tool for live two-language conversations. Two panes: the left shows
what was said (marked with a dot) in the original language, the right shows its
translation. You pick the two languages at startup. Speech comes from the
microphone or from typed text. poly never plays audio.

Nothing is written to disk. Conversation text is not logged anywhere. The
optional `-log-file` holds timings, states and errors only.

## How it works

Microphone audio goes through Silero VAD (voice activity) and, optionally,
Smart Turn (end-of-utterance detection) and a local denoiser. The finished
utterance is sent to an OpenAI-compatible speech-to-text endpoint (Whisper),
then to a chat model for translation, using the last few utterances as context.

**Language choice.** Whisper first auto-detects the language. poly checks that
the detected language is one of your two and that the text looks like it. If not,
it re-runs the audio forced to each language of the pair and keeps the better
result.

**Noise.** Coughs, keyboard clicks and the like are dropped before Whisper by:
Silero threshold 0.6, minimum speech 250 ms, trimming of the audio edges, and a
confidence floor (Whisper avg log-prob and compression ratio) on what comes back.

## Setup

Requirements: Go 1.26 with cgo enabled (a C compiler), and ALSA development
headers on Linux. The setup scripts need `curl`.

```
scripts/setup-models.sh      # Silero VAD, Smart Turn, onnxruntime
scripts/setup-denoise.sh     # optional: GTCRN denoiser model
go build -o poly ./cmd/poly  # run from the repo root
```

Default model and runtime paths (`models/`, `third_party/onnxruntime/`) are
looked up only next to the `poly` binary, never in the current directory. So
build with `go build -o poly` in the repo root as above. With `go run`, the
binary lives in a temp dir; pass `-silero-vad-model`, `-smart-turn-model`,
`-onnxruntime-lib` (and `-denoise-model`) explicitly.

### Configuration

`~/.config/poly/config.toml` (or `$XDG_CONFIG_HOME/poly/config.toml`) is loaded
automatically if it exists. A `./config.toml` is read only when you pass
`-config ./config.toml`. Copy `config.example.toml` as a starting point; it lists
every key and its matching flag. Unknown keys are an error.

Precedence: flag, then config file, then `OPENAI_API_KEY` (key only), then defaults.

- `base_url` is required (no default, so a key is never sent to a provider you
  did not choose): set it in the config file or with `-base-url`. It must be
  `https`; plain `http` is accepted only for localhost.
- Set the key with `OPENAI_API_KEY` rather than writing `api_key` in a file. Never
  commit a config that contains a key.

Example for Scaleway Generative APIs:

```toml
base_url = "https://api.scaleway.ai/<project_id>/v1"
stt_model = "whisper-large-v3"
translate_model = "mistral-small-3.2-24b-instruct-2506"
```

```
export OPENAI_API_KEY=...   # your Scaleway secret key
```

### Live preview and cost

With live preview on, poly shows provisional text and translation in both panes
while someone is speaking, and finished sentences appear before the speaker
stops. It works by re-sending audio to Whisper, so transcription cost rises
(roughly up to 4x the audio). It is on by default; turn it off with
`live_preview = false` or `-live-preview=false` to get one transcription per
turn (`preview_interval_ms`, minimum 100, is config-file only).

The status bar shows a session counter of tokens and audio duration. A price
estimate is added only when all three of `price_audio_per_min`,
`price_in_per_mtok` and `price_out_per_mtok` are set (optionally
`price_currency`); there are no default prices. `config.example.toml` lists
Scaleway figures for reference.

## Usage

```
poly -langs fr,en                 # French on the left, English on the right
poly -langs fr,en -mic off        # typed input only; no audio models loaded
poly -langs fr,en -timestamps     # show times on rows
poly -langs fr,en -translate-context 6
poly -langs fr,en -denoise        # local noise suppression (needs setup-denoise.sh)
poly -langs fr,en -log-file poly.log
```

`-langs` takes two Whisper language codes. Run `poly -h` for all flags.

### Keys

| key | action |
|-----|--------|
| Enter | send typed text |
| Esc | cancel the pending translation (cancelled rows stay, greyed out); with live preview on, also drop the sentence being spoken (its provisional line and audio); sentences already committed stay |
| Ctrl+P | mute / unmute the microphone |
| PgUp / PgDn, mouse wheel | scroll both panes |
| ? | help (when the input is empty) |
| Ctrl+C | quit |

## Evaluation

`poly-eval` runs real clips through the same pipeline as the app and reports
language-pick accuracy and noise rejection against your API.

```
go build -o poly-eval ./cmd/poly-eval
scripts/fetch-tatoeba-clips.sh fr,en,es,ja 10 eval/clips   # needs curl, ffmpeg, python3
OPENAI_API_KEY=... ./poly-eval -config my.toml -clips eval/clips/meta.json
```

Audio is not committed (Tatoeba licenses vary); only the fetch script is. See
`eval/README.md` for all options and the measured results with whisper-large-v3
(language pick 20/20 on all six tested pairs; all 25 noise clips rejected, no
short real word lost).

## Known limitations

- The live microphone path was verified only by hermetic tests in the
  development environment, which had no capture device. Expect rough edges on
  first real use.
- The denoiser has not been tested on real cafe noise.
- ALSA may print warnings to stderr before the UI starts. Redirect with
  `poly ... 2>/dev/null` if they bother you.
