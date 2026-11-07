#!/usr/bin/env bash
# Downloads N random Tatoeba sentences with audio per language and converts
# them to 48 kHz mono s16le WAV, writing OUTDIR/meta.json = [{lang,wav,ref}].
# Usage: scripts/fetch-tatoeba-clips.sh LANGS N OUTDIR      (LANGS e.g. fr,en,es,ja)
# Env: WORD_MIN (default 3), WORD_MAX (default unset) bound the word count;
#      e.g. WORD_MIN=1 WORD_MAX=2 fetches short utterances.
# Tatoeba audio licenses vary per recording: do not commit the clips.
set -euo pipefail

LANGS="${1:?usage: $0 LANGS N OUTDIR}"; N="${2:?}"; OUT="${3:?}"
WORD_MIN="${WORD_MIN:-3}"; WORD_MAX="${WORD_MAX:-}"
for c in curl ffmpeg python3; do command -v "$c" >/dev/null || { echo "error: $c required" >&2; exit 1; }; done
mkdir -p "$OUT"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

iso3() { case "$1" in fr) echo fra;; en) echo eng;; es) echo spa;; ja) echo jpn;; de) echo deu;; it) echo ita;; pt) echo por;; *) echo "error: unsupported language $1" >&2; return 1;; esac; }

: > "$WORK/meta.jsonl"
IFS=, read -ra LS <<<"$LANGS"
for lang in "${LS[@]}"; do
  code="$(iso3 "$lang")"
  got=0; tries=0
  while [ "$got" -lt "$N" ] && [ "$tries" -lt 20 ]; do
    tries=$((tries+1))
    url="https://tatoeba.org/en/api_v0/search?from=${code}&has_audio=yes&sort=random&word_count_min=${WORD_MIN}"
    [ -n "$WORD_MAX" ] && url="${url}&word_count_max=${WORD_MAX}"
    curl -fsS --retry 3 "$url" -o "$WORK/search.json" || continue
    # prints: audio_id<TAB>text for each result that has audio
    python3 -I -c '
import json,sys
for r in json.load(open(sys.argv[1])).get("results",[]):
    if r.get("audios"):
        print("%d\t%s" % (r["audios"][0]["id"], r["text"].replace("\t"," ").replace("\n"," ")))
' "$WORK/search.json" > "$WORK/hits.tsv"
    while IFS=$'\t' read -r aid text; do
      [ "$got" -ge "$N" ] && break
      wav="$OUT/${lang}-${aid}.wav"
      if [ ! -s "$wav" ]; then
        curl -fsSL --retry 3 "https://tatoeba.org/audio/download/${aid}" -o "$WORK/a.audio" || continue
        ffmpeg -nostdin -loglevel error -y -i "$WORK/a.audio" -ac 1 -ar 48000 -c:a pcm_s16le -map_metadata -1 -fflags +bitexact -flags:a +bitexact "$wav" || continue
      fi
      LANG_="$lang" WAV_="$(basename "$wav")" TEXT_="$text" python3 -I -c '
import json,os
print(json.dumps({"lang":os.environ["LANG_"],"wav":os.environ["WAV_"],"ref":os.environ["TEXT_"]},ensure_ascii=False))' >> "$WORK/meta.jsonl"
      got=$((got+1))
    done < "$WORK/hits.tsv"
  done
  echo "$lang: $got clips" >&2
done
python3 -I -c '
import json,sys
print(json.dumps([json.loads(l) for l in open(sys.argv[1])],ensure_ascii=False,indent=1))' "$WORK/meta.jsonl" > "$OUT/meta.json"
