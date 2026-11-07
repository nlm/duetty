# poly — live preview, sentence commits and cost counter

Status: draft for approval · 2026-11-07 · extends
`docs/superpowers/specs/2026-11-07-poly-design.md` (the base design; everything
not mentioned here stays as it is).

## Intent and why

Today poly sends audio only when Smart Turn decides the speaker's *turn* is
over. Someone who says three sentences in a row waits for all three before
seeing anything, and the other person reads nothing until the speaker stops.
The user wants to see the text and its translation **while the person is
speaking**, sentence by sentence, in both panes. They also want to **keep an
eye on the cost** of the session, since live preview multiplies API calls.

## Findings that shape the design (measured 2026-11-07)

All on Scaleway (`whisper-large-v3`, `mistral-small-3.2-24b-instruct-2506`),
with the 40 Tatoeba clips (fr/en/es/ja) in `eval/clips`.

1. **Audio cannot find sentence ends.** Smart Turn v3.2 said "done" (p ≥ 0.5)
   at 92 % of real sentence ends, but also at 80 % of artificial
   mid-sentence cuts (118 cuts at the deepest word gaps, + 200 ms silence).
   In real use the user saw the opposite failure (held across sentences).
   → Do not cut audio on pauses to make sentences.
2. **Whisper on a fragment invents an ending.** Mid-sentence fragments came
   back ending in `.`/`?` 32/40 times, often with invented words ("…to be
   a man." for "…to be angry."), and 2/40 got the wrong language (ja → ru,
   fr → es). → A fragment's text and language are provisional; never commit
   the last sentence of an unfinished recording.
3. **Whisper on the whole recording marks sentences well.** Two sentences
   joined by 300 ms: the text had a sentence boundary in 19/20; a separate
   `segments[]` entry with a timestamp at the junction in 15/20.
4. **Whisper is fast.** Median latency: 1–4 s audio ≈ 0.3 s, 8 s ≈ 0.5 s,
   15 s ≈ 0.7 s, 25 s ≈ 1.1 s.
5. **Partial sentences translate sensibly.** "Je voudrais réserver une table
   pour" → "I would like to book a table for".
6. **Usage is reported.** Whisper `verbose_json` returns
   `usage: {type: "duration", seconds: N}` (audio seconds, integer);
   chat completions return `usage.prompt_tokens` / `completion_tokens`.

## Goals

- While someone speaks, both panes show what has been heard so far and its
  translation, refreshed about once a second, in a "provisional" style.
- Each finished sentence becomes a normal row (gray `»` original,
  translation in the terminal's normal color) as soon as Whisper shows that a next sentence has started,
  without waiting for the end of the turn.
- Escape also drops the sentence being spoken (the live preview).
- A session cost counter in the status bar.

## Non-goals

- Streaming audio to a realtime API (Scaleway has none; `/realtime` is 404).
- Persisting usage or cost across sessions.
- Live preview for typed input (it is already instant).

## Anti-goals

- Committing text that later turns out wrong because it was a fragment
  (findings 1–2): the newest sentence of a recording that is still growing
  is never committed.
- Flicker: the provisional line must not jump between languages or panes
  on every refresh.
- A counter that hides cost: every billed call counts, including preview
  refreshes and translations that are later thrown away.

## How it works

### The live recording

The listener keeps today's endpointing (Silero + Smart Turn decide when the
turn ends). In addition, while a turn is in progress it exposes the audio
captured so far, from which already-committed audio has been removed (the
"live recording").

### Refresh loop (one per live turn)

- The live recording is sent to Whisper again once enough new audio has
  arrived: at least `live_preview_interval_ms` (default 1000), and at least a
  quarter of the live recording's length. A 4 s recording refreshes every
  second; a 20 s one that never splits refreshes every 5 s. This keeps the
  billed audio at about 4× real time at most, however long a recording
  grows without a commit. At most **one** preview transcription is in
  flight; if the speaker gets ahead, the next refresh simply covers more
  audio. (Claude's call: the cost cap.)
- Recordings shorter than the minimum speech length are not sent.
- **Language:** refreshes use the normal resolver path (auto-detect + text
  check, base design). The language is **locked** once the live recording
  holds ≥ 3 s of speech, and every later refresh of that recording is a
  single Whisper call forced to that language. **Each commit unlocks it**:
  the remaining recording is detected again from scratch. Reason: one turn
  can hold both people (the other person answers before Smart Turn ends the
  turn), and a recording forced to the wrong language comes back as a
  translation posing as an original (base design, finding 1). Unlocking at
  each commit means the answer is detected afresh once the first speaker's
  sentences are committed. The provisional line stays in the pane of the
  language shown at the previous refresh until the language is locked, to
  avoid jumping panes on every refresh. (Claude's call: the 3 s figure and
  unlock-at-commit. Known gap: speech by both people inside one segment
  that never commits stays mixed until the end of the turn, which then
  resolves it as today.)
- The noise floor of the base design applies: a refresh judged noise shows
  nothing and commits nothing.

### Commit rule

A refresh commits up to the **last segment that both ends in sentence
punctuation** (`. ? ! … 。 ？ ！`, ignoring trailing quotes/brackets) **and
is followed by another segment**. The newest segment is never committed.
Whisper also splits long sentences at clause breaks; requiring the
punctuation keeps those half-sentences from becoming rows. The committed
text is the concatenation of those segments. The live recording is trimmed
at the end time of the last committed segment, mapped back from the
16 kHz upload to the listener's own audio. (Claude's call: the punctuation
requirement.) Each committed sentence group becomes a normal
row (Seq assigned in capture order, base design) and is translated as
today, with history.

If a recording never splits into segments (5/20 in finding 3), nothing is
committed early; it is committed whole at the end of the turn, as today.

### End of turn

When Smart Turn (or the silence cap, or mute) ends the turn, the remaining
live recording is resolved and committed exactly as today's single
utterance (full resolver; language locked if already locked).

The provisional line **stays on screen**, still provisional, until that
final pass returns. It is then replaced in place by the committed row,
with no blank gap. If the pass is judged noise, the line is removed; if it
fails, it becomes the usual error row. A new turn can start while that pass
is running, so there can briefly be two provisional entries, each in
capture order. (Claude's call: model each live turn as an entry in the
conversation log with a "live" state rather than a single UI-only line.)

### Live translation

- Whenever a refresh produces provisional text **different** from the last
  one translated, it is translated (same model, same history as a normal
  row) and shown in the other pane in the provisional style.
- At most one live translation in flight; a newer provisional text replaces
  the queued one (latest wins). A live translation that comes back after its
  text was committed or replaced is discarded (but still counted for cost).
- Commits use their own normal translation (the committed text may differ
  from what the live translation saw).

### Screen

- The provisional sentence is shown at the bottom, after all rows, in both
  panes, in gray italic: original with the `»` marker on the speaker's
  side, translation on the other side. It joins the previous paragraph if
  the previous row is in the same language (same merging rule as rows).
- When the live translation lags, the other side shows the last
  translation it has plus `…`.
- Committed rows look exactly as today.

### Escape

Escape cancels pending translations (as today) **and** drops the live
turn: the provisional line disappears, the captured audio is discarded, any
in-flight preview transcription or live translation is ignored when it
returns, and listening restarts fresh (speech that continues after Escape
starts a new turn). Already-committed sentences of that turn stay.

### Cost counter

- Counted per session, in memory, from the API `usage` fields of **every**
  call: Whisper audio seconds (preview refreshes, forced passes, final
  passes) and chat prompt/completion tokens (live and committed
  translations, cancelled or discarded ones included when the response
  arrived).
- Shown at the right of the status bar, e.g. `18.3k tok · 4m10s audio`.
- Prices are optional config keys **with no defaults**:
  `price_audio_per_min`, `price_in_per_mtok`, `price_out_per_mtok`,
  `price_currency` (a label such as `€`). When all three prices are set the
  status bar also shows the estimate first, e.g.
  `≈ €0.042 · 18.3k tok · 4m10s audio`. When any is missing, no price is
  shown. (For reference only, not as defaults: Scaleway lists
  whisper-large-v3 at €0.003/min, mistral-small-3.2 at €0.15 / €0.35 per
  million tokens in/out.)
- A response without `usage` counts nothing for that call (no guessing).

### Configuration added

| key | flag | default |
|---|---|---|
| `live_preview` | `-live-preview` | `true` |
| `live_preview_interval_ms` | — | `1000` |
| `price_audio_per_min` | — | unset |
| `price_in_per_mtok` | — | unset |
| `price_out_per_mtok` | — | unset |
| `price_currency` | — | `""` |

Live preview is **on by default** (the user changed this on 2026-11-07;
it was first specified as off by default). `live_preview = false` or
`-live-preview=false` turns off everything in "How it works" above except
the cost counter, which is always on: poly then behaves exactly as before
(one transcription per turn, no early commits), with the cost counter
still shown. Escape dropping the live turn only
applies when live preview is on.

### Expected cost (for the user, not a requirement)

Continuous speech with commits keeping the live recording around 5 s:
Whisper ≈ 4× today's audio, ≈ €0.012 per minute of speech at Scaleway
prices; live translations ≈ €0.002 per minute.

## Constraints carried over

All base-design constraints and security rulings stay: no audio or text
persisted, key never logged, https-only base URL, the log file records
timings and counts only (never text).

## Left to the builder

- How the listener exposes the live recording (callback, snapshot method)
  and how trimming is coordinated with the hold logic.
- Where usage is accumulated (a small counter shared by the STT and
  translate clients is suggested) and the exact status bar formatting
  (`k` for thousands, `m`/`s` for audio).
- Tests: fakes for Whisper segments, refresh pacing, commit/trim, Escape
  mid-turn, stale live translations, counter with and without prices.
- An eval addition is welcome: replay the joined-pair clips through the
  live path at real speed and report commits and words lost/duplicated.
