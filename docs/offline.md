# Running duetty offline

duetty only needs two OpenAI-compatible endpoints: one for speech-to-text and
one for translation. Both can run on your own machine, so the conversation
never leaves it, no API key is needed and nothing is billed.

This guide uses two open-source servers in Docker:

- [speaches](https://github.com/speaches-ai/speaches) for speech-to-text
  (Whisper, through faster-whisper)
- [Ollama](https://ollama.com) for translation

Native installs of either work the same way; only the URLs matter.

## 1. Speech-to-text: speaches

```sh
docker run -d --name duetty-stt -p 127.0.0.1:8000:8000 \
  -v speaches-cache:/home/ubuntu/.cache/huggingface \
  ghcr.io/speaches-ai/speaches:latest-cpu

# download a Whisper model into the server (once)
curl -X POST localhost:8000/v1/models/Systran/faster-whisper-small
```

With an NVIDIA GPU, use the `latest-cuda` image and add `--gpus=all`, then
pick a larger model such as `Systran/faster-whisper-large-v3` for better
accuracy.

## 2. Translation: Ollama

```sh
docker run -d --name duetty-llm -p 127.0.0.1:11434:11434 \
  -v ollama:/root/.ollama ollama/ollama

docker exec duetty-llm ollama pull qwen2.5:3b
```

Add `--gpus=all` with an NVIDIA GPU. Translation quality depends mostly on the
model: a 3B model is usable but clumsy (see below); a 7B model or larger is
worth it if your machine can run it.

## 3. Point duetty at them

In `~/.config/duetty/config.toml`:

```toml
stt_api_url       = "http://localhost:8000/v1"
stt_model         = "Systran/faster-whisper-small"
translate_api_url = "http://localhost:11434/v1"
translate_model   = "qwen2.5:3b"
```

`api_url` and the API key are not needed when both endpoints are on
`localhost`. Plain `http` is accepted only for `localhost`, `127.0.0.1` and
`::1`.

You can also mix: a local Whisper server with a cloud translator, or the other
way round. Set the one local endpoint and leave `api_url` and your key for the
other.

## What to expect

Measured on 2026-11-08 on an 8-core CPU (AMD EPYC), no GPU, with
`faster-whisper-small` and `qwen2.5:3b`:

- **Language pick:** `duetty-eval` picked the right language for 20/20 clips
  on each of the six pairs tested (fr, en, es, ja), the same as
  `whisper-large-v3` in the cloud.
- **Transcription speed:** about 2.2 s per request, whatever the clip length
  (1 to 5 s of audio). Live preview still works, but the provisional text
  updates about every 2 s instead of every second. On a slow machine, set
  `live_preview = false`.
- **Translation:** "Je voudrais réserver une table pour deux personnes ce
  soir." became "I would like to reserve a table for two people this
  evening.", but "That sounds great, see you at eight." became the awkward
  "Ça sonne parfait, voir à huit." A larger model fixes most of this.
- **Disk:** about 3 GB for the speaches image and 1 GB for the small Whisper
  model; about 8.5 GB for the Ollama image and 2 GB for `qwen2.5:3b`.

The status bar still counts tokens and audio seconds. Without `price_*` keys,
no cost is shown.

## Stopping

```sh
docker rm -f duetty-stt duetty-llm          # the downloaded models stay in the volumes
docker volume rm speaches-cache ollama      # to delete them as well
```
