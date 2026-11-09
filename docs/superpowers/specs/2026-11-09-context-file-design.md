# duetty — context file, transcript correction and Whisper hints

Status: draft for approval · 2026-11-09 · extends
`docs/superpowers/specs/2026-11-07-duetty-design.md` and
`docs/superpowers/specs/2026-11-07-duetty-live-preview-design.md` (everything
not mentioned here stays as it is).

## Intent and why

Conversations full of technical terms, brands, product names and people's
names come out wrong. Usually Whisper mishears them first ("scale way" for
"Scaleway"), and the translator then faithfully translates the mistake, or
translates a name that should have stayed as it is.

The user wants to give duetty a **context file** that describes the setting
of the conversation: what it is about, the keywords, concepts, people and
brands that come up. duetty uses it live to:

1. help Whisper hear those words right in the first place;
2. let the translation step **correct what was heard**, both the special
   words and obvious mishearings of ordinary words, so the fix shows in
   *both* panes, not only in the translation;
3. translate terms and names the way the context says.

The file is written before the meeting and usually describes a *setting*
(e.g. "meetings with the storage team"), reused across many conversations.

The user also wants a model to be able to **research the topics on the web
and write the file**. That is done outside duetty, by any agent that can
browse, using a prompt duetty ships.

## Goals

- An optional context file, loaded at launch, in a documented format that is
  easy to write by hand and easy for an agent to produce.
- Live correction of the speaker's text by the translation step, with or
  without a context file.
- A compact Whisper hint generated from the context file.
- A translation prompt ordered so the context benefits from the provider's
  automatic prefix (prompt) caching.
- A shipped research prompt and a short guide for using it with any agent.
- Every part optional and tunable (flag and/or config key), **enabled by
  default**, unless a measurement below fails (then off by default).

## Non-goals

- No web search, scraping or research loop inside duetty.
- No post-meeting cleanup of a whole transcript; correction is live, sentence
  by sentence.
- No new endpoints or providers: everything goes through the existing
  speech-to-text and translation endpoints.

## Anti-goals (would make this a failure even if it works)

- Corrections that rewrite *what the person meant*: rephrasing, polishing
  grammar, adding or dropping content. Correction fixes mishearings only.
- Losing a sentence because the model's reply was malformed. Always fall back
  to the text as heard plus a plain translation.
- Writing conversation text to disk. The privacy promise in the README holds;
  the context file is read, never written.
- Breaking the live preview's commit logic (see "Live preview" below).

## Parts

### 1. The context file

- Loaded with `-context PATH` / `context_file = "PATH"` in config.toml.
  Default: none. Missing or unreadable path → startup error (it was asked for
  explicitly).
- Format: **Markdown**, free text with a few recognised sections. Suggested
  shape (documented in `docs/context.md`, used by the research prompt):

  ```markdown
  # Setting
  One or two paragraphs: who meets, about what, in which company/field.

  ## People
  - Jane Doe — CTO of Acme (name stays as is)

  ## Brands and products
  - Scaleway — cloud provider; never translated
  - Object Storage — keep in English in French conversations

  ## Terms
  - bucket — fr: « bucket » (not « seau »)

  ## Whisper hint
  (optional) text used verbatim as the Whisper hint, one per language:
  ### fr
  ...
  ### en
  ...
  ```

- duetty does **not** need to parse most of it: the whole file (minus the
  `Whisper hint` section) goes to the translator as is. Only the `Whisper
  hint` section is recognised structurally.
- Size: no hard format limit, but duetty warns at startup when the file is
  large (threshold left to the builder, e.g. > ~2 000 tokens), since it is
  sent with every translation request.

### 2. Whisper hint

- Sent as the `prompt` field of each transcription request, including live
  preview passes and the per-language re-transcriptions in `stt.Resolver`.
- **One hint per language** of the pair; the hint matching the language being
  transcribed is sent. **No hint on the auto-detect pass** (no language
  set): a prompt biases Whisper's language guess towards the prompt's
  language, and language pick is calibrated without one (my call).
- **No regression allowed** on the existing eval suites (language pick
  20/20 per pair, noise rejection as in `eval/README.md`) with the hint on.
  Whisper can echo its prompt back on silence or noise: if the noise suite
  regresses, fix it (e.g. drop results that repeat the hint) rather than
  re-tune the calibrated thresholds (my call).
- **Source of the hints:**
  - if the file has a `Whisper hint` section, use it verbatim (no call);
  - otherwise duetty **generates them at startup** with one call to the
    translation model: given the context file, write for each language a
    short natural sentence or two that uses the hardest words first (names,
    brands, unusual spellings), within Whisper's limit (~224 tokens; target
    well under, e.g. ≤ 150 words). Natural sentences, not bare comma lists,
    because Whisper copies the style of its prompt.
  - Generation failure → warn in the status bar/log and run without a hint.
- Generated hints are kept in memory only. A way to see them is useful for
  tuning (e.g. `-print-whisper-hint` that prints and exits, so the user can
  paste it into the file) — builder's call on the exact form.
- Switch: `-whisper-hint` / `whisper_hint = true` (default true; no effect
  without a context file).

### 3. Correction of the speaker's text

- The translation call returns **two things**: the corrected original and its
  translation. Output format is the builder's call (e.g. JSON object, or two
  delimited lines); it must survive models like mistral-small and local
  Ollama models. Malformed reply → retry is not required; fall back to:
  original as heard + the translation if one can be extracted, else a
  translation-only request.
- Scope: fix mishearings of context terms **and** obvious mishearings of
  ordinary words. Never rephrase. The instruction must say so firmly.
- Works with or without a context file.
- The speaker's pane shows the corrected text **with no marker** (my call;
  easy to change).
- The corrected text is what goes into later requests' history.
- **Typed input is not corrected** (my call: nothing was misheard; typed text
  still gets the context for translation).
- Switch: `-correct` / `correct = true` (default true). Off → today's
  behaviour exactly (translation-only output, original as heard).

### 4. Translation prompt and caching

- Message order, so the longest stable part is a byte-identical prefix for
  every request of the session, in **both** directions:
  1. system: fixed instructions (independent of direction) + the context file;
  2. then the direction ("French → English") and output-format reminder;
  3. history pairs, then the new sentence.
- Today `systemPrompt(from, to)` puts the direction first, which breaks the
  prefix on every speaker change; that moves out of the opening.
- History stays as it is (`translate_context`, default 4), placed after the
  stable prefix.
- No provider-specific caching API: rely on automatic prefix caching
  (vLLM/Ollama/most OpenAI-compatible servers). If the provider reports cached
  tokens (`usage.prompt_tokens_details.cached_tokens`), count them in the
  usage meter and log; showing it in the status bar is the builder's call.

### 5. Research prompt (outside duetty)

- Ship `docs/context-research-prompt.md`: a prompt the user gives to any agent
  that can browse (Claude, ChatGPT, Perplexity…) together with their keywords,
  people and brands and the two languages. The agent searches the web and
  returns a context file in the format above, including a `Whisper hint`
  section.
- The prompt asks for: correct spellings, short one-line descriptions, how
  each term should be rendered in each language (keep / translate as X), and
  sources it relied on removed from the final file (keep the file lean).
- `docs/context.md` documents the format and how to use the prompt; README
  gets a short section and a link.

## Live preview

- Live preview's commit rules (`internal/live/rules.go`) work on Whisper's own
  text and segments. Correction must **not** feed back into them: keep the
  heard text for the commit logic and the staleness checks
  (`transText` vs `Original` in `convo`), and show the corrected text for
  display. How to split the fields is the builder's call.
- Previews get corrected text too when correction is on, since they come from
  the same call.

## Config summary

| flag | config key | default |
|---|---|---|
| `-context PATH` | `context_file` | none |
| `-whisper-hint` | `whisper_hint` | true |
| `-correct` | `correct` | true |

Add them to `config.example.toml` in the existing style.

## Things to measure (before calling it done)

Run these early: their results decide defaults.

- **Correction doesn't rephrase.** Extend `duetty-eval` to run the
  translation step (with correction) on the existing Tatoeba clips and
  report, per clip, heard text vs corrected text. Pass = corrected equals
  heard, ignoring case, whitespace and punctuation, on every clip where the
  heard text was already right; any word-level change on such a clip is a
  rephrase. Measure with mistral-small on Scaleway and with the model in
  `docs/offline.md`.
- **Latency** added per sentence by the two-part output (median over the
  same clips, correction on vs off).
- **Hints are accepted and harmless.** Whether Scaleway's `whisper-large-v3`
  and speaches honour the `prompt` field; the existing language and noise
  suites re-run with a hint (see part 2: no regression allowed).
- **Hints fix names.** No brand-name clips exist in the repo, so this is a
  manual check by the user with real speech and a real context file; the
  builder documents how in `docs/context.md`. Not automated (decided).
- **Caching.** Whether Scaleway's chat endpoint reports cached tokens /
  speeds up with a stable prefix (information only).

**If a measurement fails, that part still ships but is off by default**
(decided). Correction fails if it rephrases on any clip with a given model,
or adds latency the builder judges noticeable (record the numbers). The
Whisper hint fails if the language or noise suites regress and a simple fix
doesn't recover them. The doc and `config.example.toml` say what was
measured and why the default is what it is.

## Left to the builder

Output format for the two-part reply; exact prompt wording; size warning
threshold; hint-generation prompt; how to expose the generated hint; field
split between heard and shown text; test design.
