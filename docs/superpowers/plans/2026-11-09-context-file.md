# Context File, Live Correction and Whisper Hints Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let duetty load an optional Markdown context file, use it for Whisper hints and translation, and have the translation step correct the speaker's text live (changed words in blue), all switchable and on by default.

**Architecture:** A small `ctxfile` package parses the file into a translator body and optional per-language hints. `translate` gets a request/result interface, a cache-friendly prompt order, a JSON two-part reply with fallback, and a one-shot hint generator. `stt` sends a per-language `prompt`. `convo` stores the corrected text next to the heard text (heard text keeps driving all live-preview logic), and `ui` colours the changed words. `duetty-eval` grows a correction/latency mode and a hint option, used to measure before defaults are final.

**Tech Stack:** Go 1.26, existing deps only (BurntSushi/toml, tview, go-runewidth). No new modules.

**Spec:** `docs/superpowers/specs/2026-11-09-context-file-design.md`

## Global Constraints

- Every new switch has a flag and a config key; defaults: `context_file` none, `whisper_hint` true, `correct` true, `mark_corrections` true.
- `correct = false` must give exactly today's behaviour: translation-only request, text as heard.
- Correction never rephrases; a malformed reply never loses a sentence (fall back to heard text + plain translation).
- No conversation text, hint or context content written to disk or to the log file (log keeps timings, counts, errors only).
- No hint on the auto-detect Whisper pass (`language == ""`).
- Hints ≤ 150 words each (truncate generated ones).
- Translation request order: [system: fixed direction-independent instructions + context body] → [system: direction + output format] → history → new text. The first message is byte-identical for every request of a session.
- Typed input is never corrected (still translated with context).
- `config.example.toml` lists every key exactly once (`TestExampleConfigListsEveryKey`).
- User-facing docs (README, `docs/context.md`, `docs/context.example.md`, research prompt, final `config.example.toml` wording) are written last, in Task 11.
- Run `go test ./...` and `go vet ./...` green before each commit.

## Review Focus

1. Model wraps its JSON in ```` ```json ```` fences or adds a sentence before it → still parsed (Task 3 test `TestCorrectReplyFenced`).
2. Model "corrects" into a different sentence (hallucination, rephrase) → correction discarded, heard text shown (Task 3 test `TestCorrectionTooDifferentDiscarded`).
3. Japanese/Chinese text has no spaces → diff by rune for wide runes, so only changed characters turn blue (Task 7 test `TestCorrectionSegsCJK`).
4. Live preview: a correction made for older text must not hide newer heard words → `Corrected` cleared while stale (Task 6 test `TestLiveCorrectedClearedWhenStale`).
5. Context file saved with a UTF-8 BOM and CRLF line endings → hint section still found, body clean (Task 1 test `TestLoadBOMAndCRLF`).

---

### Task 1: `ctxfile` package

**Files:**
- Create: `internal/ctxfile/ctxfile.go`
- Test: `internal/ctxfile/ctxfile_test.go`

**Interfaces:**
- Produces:
  - `type File struct { Body string; Hints map[string]string }`: `Body` is the whole file minus the `Whisper hint` section, trimmed. `Hints` maps lowercase language code → hint text, nil when there's no section.
  - `func Load(path string) (File, error)`: read error wrapped as `context file: ...`.
  - `func Parse(src string) File`
  - `func (f File) ApproxTokens() int` = rune count / 4
  - `const WarnTokens = 2000`

Parsing rules: strip a leading BOM and normalise CRLF to LF. The hint section starts at a heading line (`#`–`###`) whose text, trimmed and case-folded, equals `whisper hint`. It runs until the next heading of the same or a higher level. Inside it, each `#{level+1} <code>` sub-heading starts that language's hint (code trimmed and lowercased). Text before the first sub-heading is ignored.

- [ ] **Step 1: Write failing tests**
  - `TestParseNoHintSection`: `Parse("# Setting\nA\n")` → `Body == "# Setting\nA"`, `Hints == nil`.
  - `TestParseHintSection`: the spec appendix example (copy it into `testdata/example.md`) → `Hints["fr"]` starts with `"Réunion entre Scaleway"`, `Hints["en"]` starts with `"Meeting between Scaleway"`, `Body` contains `"## Terms"` and not `"Whisper hint"`.
  - `TestParseHintSectionNotLast`: hint section followed by `## Notes\nX` → `Body` contains `"## Notes\nX"`.
  - `TestLoadBOMAndCRLF`: file with `"﻿# S\r\n## Whisper hint\r\n### FR\r\nBonjour\r\n"` → `Hints["fr"] == "Bonjour"`, `Body == "# S"`.
  - `TestLoadMissing`: error message contains `"context file"`.
  - `TestApproxTokens`: 8000-rune body → 2000.
- [ ] **Step 2: Run** `go test ./internal/ctxfile/`. Expected: FAIL (package missing).
- [ ] **Step 3: Implement** `Parse`, `Load` and `ApproxTokens` in `internal/ctxfile/ctxfile.go`, scanning line by line.
- [ ] **Step 4: Run** `go test ./internal/ctxfile/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(ctxfile): parse context file and Whisper hint section`

---

### Task 2: Config keys and flags

**Files:**
- Modify: `internal/config/config.go` (Config struct, `Defaults()`, `newFlagSet`, the flag→field map in `Load`)
- Modify: `internal/config/example_test.go` (skip fields tagged `toml:"-"`)
- Modify: `config.example.toml` (one entry per new key, existing style; final wording is polished in Task 11)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces Config fields:
  - `ContextFile string` (`toml:"context_file"`, flag `-context`)
  - `WhisperHint bool` (`whisper_hint`, `-whisper-hint`, default true)
  - `Correct bool` (`correct`, `-correct`, default true)
  - `MarkCorrections bool` (`mark_corrections`, `-mark-corrections`, default true)
  - `PrintWhisperHint bool` (`toml:"-"`, flag `-print-whisper-hint` only: "print the Whisper hints for -context and exit")

- [ ] **Step 1: Write failing tests**
  - `TestContextDefaults`: `Defaults()` has `WhisperHint`, `Correct` and `MarkCorrections` true, and `ContextFile == ""`.
  - `TestContextFlags`: `Load([]string{"-context","x.md","-correct=false","-whisper-hint=false","-mark-corrections=false","-print-whisper-hint"}, noenv)` sets all five fields.
  - `TestContextFileKeys`: a TOML file with `context_file = "c.md"`, `correct = false` loads them. A flag still wins over the file.
- [ ] **Step 2: Run** `go test ./internal/config/`. Expected: FAIL.
- [ ] **Step 3: Implement** the fields, defaults and flags, plus the entries in the flag-set map. Make the example test skip `-` tags. Add the four keys to `config.example.toml` under a new `# --- Context ---` section, commented out with their defaults.
- [ ] **Step 4: Run** `go test ./internal/config/`. Expected: PASS, including `TestExampleConfigListsEveryKey`.
- [ ] **Step 5: Commit** `feat(config): context file, whisper hint, correction switches`

---

### Task 3: Translator: request/result interface, prompt order, correction

**Files:**
- Modify: `internal/translate/translate.go`
- Modify: `internal/usage/usage.go` (cached tokens)
- Modify: every `Translator` fake and caller: `internal/convo/convo.go` (two call sites, mechanical only; the behaviour comes in Task 6), `internal/convo/*_test.go`, `internal/app/run_test.go`
- Test: `internal/translate/translate_test.go`, `internal/usage/usage_test.go`

**Interfaces:**
- Produces:
  ```go
  type Request struct {
      Text, From, To string
      History []Pair
      Correct bool // ask for the corrected original too
  }
  type Result struct {
      Translation string
      Corrected   string // "" when not asked, unusable or discarded
  }
  type Translator interface {
      Translate(ctx context.Context, req Request) (Result, error)
  }
  func (c *Client) SetContext(body string) // call before concurrent use
  ```
  - `Pair` keeps `Original` as the text to show (Task 6 passes the corrected text).
  - `usage.Totals.CachedTokens int64` and `func (m *Meter) AddCached(n int64)`.

**Prompt.**
- Message 1 (system) is a constant instruction, with the context appended under a `Context:` line when one is set. The instruction says:
  - you translate between two languages in a conversation;
  - names, brands and terms follow the context;
  - when asked to correct, fix only words misheard by speech recognition, never rephrase, add or drop content.
- Message 2 (system) is `"Translate from <From> to <To>."`, plus the format line.
  - Correct mode: `Reply with JSON only: {"corrected": "<the sentence as the speaker said it, with misheard words fixed>", "translation": "<its translation>"}.`
  - Otherwise: `Output only the translation, nothing else. No quotes, no notes.`
- History stays as user/assistant pairs. In correct mode, assistant turns are rendered as the JSON object, with `corrected` set to the user text.

**Reply handling in correct mode.**
1. Take the substring from the first `{` to the last `}` and `json.Unmarshal` it.
2. If that fails or `translation` is empty, send one translation-only request (Correct=false) and return it with `Corrected ""`.
3. Apply `cleanReply` to the translation.
4. Discard `corrected` (set it to "") if it is empty, or if `similarity(heard, corrected) < 0.5`. `similarity` = 1 − Levenshtein(a, b) / max(len(a), len(b)) over runes, where a and b are the texts case-folded with punctuation and all whitespace removed. So "scale way" vs "Scaleway" scores 1.0 (a split word is kept), and an unrelated sentence scores low.

`response.Usage` gains `PromptTokensDetails struct{ CachedTokens int64 }` (`json:"prompt_tokens_details"`). A non-zero value goes to `meter.AddCached`.

- [ ] **Step 1: Write failing tests** with an `httptest` server capturing the request body. Each one is driven only through the public `Translate`:
  - `TestPrefixStableAcrossDirections`: with `SetContext("CTX")`, requests fr→en and en→fr with different history have byte-identical `messages[0]`, and it contains `"CTX"`.
  - `TestNoContextNoContextLine`: without SetContext, `messages[0]` doesn't contain `"Context:"`.
  - `TestCorrectReply`: reply `{"corrected":"Scaleway is great","translation":"Scaleway est super"}` for text `"scale way is great"` → `Result{"Scaleway est super","Scaleway is great"}`.
  - `TestCorrectReplyFenced`: same JSON wrapped in ```` ```json ```` fences with a leading sentence → same result.
  - `TestCorrectMalformedFallsBack`: first reply `"oops"`, second reply `"Bonjour"` → `Result{"Bonjour",""}`, 2 requests made, and the second has no JSON format line.
  - `TestCorrectionTooDifferentDiscarded`: heard `"we ship on friday"`, corrected `"the cat sat on the mat today"` → `Corrected == ""`, translation kept.
  - `TestShortSplitWordKept`: heard `"scale way"`, corrected `"Scaleway"` → `Corrected == "Scaleway"`.
  - `TestPlainModeUnchanged`: `Correct:false` → the last system message says `Output only the translation`, and the reply is used as before (the existing quote-stripping tests still pass, adapted to `Request`).
  - `TestCachedTokensMetered`: usage with `prompt_tokens_details.cached_tokens: 7` → `meter.Totals().CachedTokens == 7`.
- [ ] **Step 2: Run** `go test ./internal/translate/ ./internal/usage/`. Expected: FAIL.
- [ ] **Step 3: Implement** the new types, `SetContext`, the prompt builder, the reply parser, the fallback, `similarity`, and `AddCached`.
- [ ] **Step 4: Update callers and fakes.** In `convo.go`, call `Translate(ctx, translate.Request{Text: text, From: from, To: to, History: history})` and use `res.Translation` (no correction yet). Fakes return `Result{Translation: ...}`.
- [ ] **Step 5: Run** `go test ./...`. Expected: PASS.
- [ ] **Step 6: Commit** `feat(translate): cache-friendly prompt, context and live correction`

---

### Task 4: Whisper hint generation

**Files:**
- Modify: `internal/translate/translate.go` (or a new `internal/translate/hints.go`)
- Test: `internal/translate/hints_test.go`

**Interfaces:**
- Consumes: the `Client` from Task 3.
- Produces: `func (c *Client) WhisperHints(ctx context.Context, contextBody string, langs [2]string) (map[string]string, error)`

It makes one chat request. The system prompt asks for JSON `{"<code1>": "...", "<code2>": "..."}`: for each language, one or two natural sentences in that language using the context's hardest words first (names, brands, unusual spellings), at most 150 words. The reply is parsed the same way as Task 3 (first `{` to last `}`). Only the two pair codes are kept, each truncated to 150 words, and empty values are dropped. It returns an error if neither language survives. Tokens are metered.

- [ ] **Step 1: Write failing tests**
  - `TestWhisperHintsParsed`: reply `{"fr":"Réunion Scaleway","en":"Scaleway meeting","de":"x"}` → map with exactly `fr` and `en`.
  - `TestWhisperHintsTruncated`: a 400-word value → 150 words.
  - `TestWhisperHintsEmptyIsError`: reply `{}` → error.
  - `TestWhisperHintsHTTPError`: a 500 status → error whose text doesn't contain the API key.
- [ ] **Step 2: Run** `go test ./internal/translate/ -run WhisperHints`. Expected: FAIL.
- [ ] **Step 3: Implement** `WhisperHints`.
- [ ] **Step 4: Run.** Expected: PASS.
- [ ] **Step 5: Commit** `feat(translate): generate Whisper hints from the context`

---

### Task 5: Whisper `prompt` field

**Files:**
- Modify: `internal/stt/client.go`
- Test: `internal/stt/client_test.go`

**Interfaces:**
- Produces: `func (c *Client) SetHints(h map[string]string)`. Call it before concurrent use; nil means no hints.

`Transcribe` adds the multipart field `prompt` = `h[language]` only when `language != ""` and the hint is non-empty.

- [ ] **Step 1: Write failing tests** (use the existing `serve` helper):
  - `TestHintSentForLanguage`: hints `{"fr":"Scaleway"}`, `Transcribe(..., "fr")` → form `prompt == "Scaleway"`.
  - `TestNoHintOnAutoPass`: same hints, language `""` → no `prompt` field.
  - `TestNoHintOtherLanguage`: language `"en"` → no `prompt` field.
- [ ] **Step 2: Run** `go test ./internal/stt/`. Expected: FAIL.
- [ ] **Step 3: Implement** it.
- [ ] **Step 4: Run.** Expected: PASS.
- [ ] **Step 5: Commit** `feat(stt): per-language Whisper hint`

---

### Task 6: Conversation log keeps the corrected text

**Files:**
- Modify: `internal/convo/convo.go`
- Test: `internal/convo/convo_test.go`, `internal/convo/live_test.go`

**Interfaces:**
- Consumes: `translate.Request` and `translate.Result` (Task 3).
- Produces:
  - `Row.Corrected string`: the text to show in place of `Original` when it isn't "". `Original` stays the heard text, which all live and staleness logic keeps using.
  - `func (l *Log) SetCorrect(on bool)`: call before use; default false, so existing tests keep today's behaviour.

**Rules:**
- Rows from `SubmitAudio`, `CommitLive` and `FinishLive`, and live translations, send `Correct: l.correct`. Rows from `SubmitText` always send `Correct: false` (mark them with an `entry.typed` flag).
- On success, `Corrected = res.Corrected` unless it equals `Original`, in which case it stays "".
- `history()` puts `Corrected` (if set) into `Pair.Original`.
- Live: `liveTranslated` sets `Corrected` along with `Translation`. `UpdateLive` sets `Corrected = ""` whenever the result is stale (`liveStale(e)`). `CommitLive` and `finished` clear it like `Translation`.

- [ ] **Step 1: Write failing tests** (fake translator that records requests and returns a set `Result`):
  - `TestAudioRowCorrected`: SetCorrect(true), audio resolves to `"scale way rocks"`, the fake returns `{Translation:"…", Corrected:"Scaleway rocks"}` → Row `Original "scale way rocks"`, `Corrected "Scaleway rocks"`, and the request had `Correct:true`.
  - `TestTypedNotCorrected`: SetCorrect(true), `SubmitText("hello")` → request `Correct:false`, `Corrected ""`.
  - `TestCorrectOffSendsPlain`: SetCorrect(false) → request `Correct:false`.
  - `TestHistoryUsesCorrected`: the second request's `History[0].Original == "Scaleway rocks"`.
  - `TestCorrectedEqualToOriginalIgnored`: `Corrected` equal to the heard text → Row.Corrected `""`.
  - `TestLiveCorrectedClearedWhenStale`: a live entry gets a translation with Corrected for text A. Then `UpdateLive` with text B → Row `Corrected ""` and `Stale` true.
- [ ] **Step 2: Run** `go test ./internal/convo/`. Expected: FAIL.
- [ ] **Step 3: Implement** it.
- [ ] **Step 4: Run** `go test -race ./internal/convo/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(convo): keep the corrected text next to the heard text`

---

### Task 7: Blue correction marking in the panes

**Files:**
- Modify: `internal/ui/layout.go`, `internal/ui/app.go`
- Create: `internal/ui/correct.go`
- Test: `internal/ui/correct_test.go`, `internal/ui/layout_test.go`

**Interfaces:**
- Consumes: `Row.Corrected` (Task 6).
- Produces:
  - `Layout(rows []convo.Row, pair [2]string, width int, timestamps, markCorrections bool)`: update the existing call sites and tests.
  - `func (a *App) SetMarkCorrections(on bool)` (call before Run).
  - `func correctionSegs(heard, corrected, base, mark string) []seg`

**Rules:**
- The source text shown is `Corrected` when it's not "", else `Original`.
- With marking on and `Corrected != ""`, the source segments come from `correctionSegs`. Tokens split on spaces, and a token containing wide runes is split into single runes. An LCS over normalised tokens (case-folded, punctuation stripped) marks the corrected tokens that aren't in the LCS. Kept tokens get `base`, changed ones get `mark`.
- Styles: `corrStyle = "[blue]"` for Original/Done rows, `liveCorrStyle = "[blue::i]"` for Live rows. Cancelled and Failed rows show `Original` unmarked.

- [ ] **Step 1: Write failing tests**
  - `TestCorrectionSegs`: `("scale way is great","Scaleway is great")` → segs `[{mark,"Scaleway"},{base,"is great"}]` (adjacent same-style tokens merged with spaces).
  - `TestCorrectionSegsCJK`: `("今日はスケールウェイ","今日はScaleway")` → only `"Scaleway"` in `mark`.
  - `TestCorrectionSegsCaseOnly`: `("scaleway","Scaleway")` → no marked token (case-folded equal), but the shown text is `"Scaleway"`.
  - `TestLayoutMarksCorrection`: a Done row with Corrected → the left line contains `"[blue]Scaleway"`.
  - `TestLayoutMarkOff`: same with `markCorrections=false` → shows `"Scaleway"`, no `"[blue]"`.
- [ ] **Step 2: Run** `go test ./internal/ui/`. Expected: FAIL.
- [ ] **Step 3: Implement** it. `wrapSegs` already handles multiple segments.
- [ ] **Step 4: Run** `go test ./internal/ui/`. Expected: PASS. Regenerate nothing: the README screenshot test must still pass unchanged (no corrections in its rows).
- [ ] **Step 5: Commit** `feat(ui): mark corrected words in blue`

---

### Task 8: Wire it up in the app

**Files:**
- Modify: `internal/app/run.go`, `cmd/duetty/main.go`
- Create: `internal/app/context.go`
- Test: `internal/app/context_test.go`, `internal/app/run_test.go`

**Interfaces:**
- Consumes: Tasks 1–7.
- Produces:
  - `type hintSource interface { WhisperHints(ctx context.Context, body string, langs [2]string) (map[string]string, error) }`
  - `func resolveHints(ctx context.Context, f ctxfile.File, pair [2]string, gen hintSource) (hints map[string]string, generated bool, err error)`: if `f.Hints` has any pair language, use the file's hints for those languages (no call). Otherwise call `gen` with a 30 s timeout.
  - `func PrintWhisperHints(ctx context.Context, cfg config.Config, w io.Writer) error`: needs `-context`. Prints `### <code>\n<hint>\n\n` per pair language, so it pastes straight into the file's `## Whisper hint` section.

**Run changes (only when the real clients are built, i.e. `d.Translator == nil` / `d.Resolver == nil`):**
- If `cfg.ContextFile != ""`:
  - `ctxfile.Load`; an error is returned as a startup error.
  - If `ApproxTokens() > ctxfile.WarnTokens`, `log.Warnf("context file is large (~%d tokens), sent with every translation", n)`.
  - `tc.SetContext(f.Body)`.
  - If `cfg.WhisperHint`, `resolveHints`. On error, add the notice `"whisper hint off: generation failed"` and `log.Warnf` the short error. On success, `sc.SetHints`.
  - Log only counts (hint word counts and whether they were generated), never the text.
- At exit, log the session's token totals including `CachedTokens` (counts only).
- `cl.SetCorrect(cfg.Correct)`. Also call `u.SetMarkCorrections(cfg.MarkCorrections)` when the UI is the real `*ui.App`. Extend the test `UI` interface if that's simpler.
- `main.go`: if `cfg.PrintWhisperHint`, run `PrintWhisperHints` to stdout and exit (code 1 with the error on failure).

- [ ] **Step 1: Write failing tests**
  - `TestResolveHintsFromFile`: a File with `Hints{"fr":"a","en":"b"}` → the fake gen isn't called, hints returned, `generated == false`.
  - `TestResolveHintsGenerated`: no hints → gen called once with `f.Body`, `generated == true`.
  - `TestResolveHintsError`: gen errors → error returned.
  - `TestRunMissingContextFile`: `cfg.ContextFile = "nope.md"` → `Run` returns an error containing `"context file"` before the UI starts.
  - `TestRunSetsCorrect`: with a fake translator, an audio turn's request has `Correct:true` by default, and `Correct:false` with `cfg.Correct = false`.
- [ ] **Step 2: Run** `go test ./internal/app/`. Expected: FAIL.
- [ ] **Step 3: Implement** it.
- [ ] **Step 4: Run** `go test ./...` and `go vet ./...`. Expected: PASS.
- [ ] **Step 5: Manual smoke test** (needs an API key): `./duetty -mic off -context internal/ctxfile/testdata/example.md`. Type `scale way object storage`: the text shows as typed (typed text isn't corrected), and the translation keeps "Object Storage". Then run `./duetty -print-whisper-hint -context internal/ctxfile/testdata/example.md`: it prints the file's hints.
- [ ] **Step 6: Commit** `feat(app): load context file, hints and correction switches`

---

### Task 9: `duetty-eval`: correction, latency and hint modes

**Files:**
- Modify: `cmd/duetty-eval/main.go`
- Create: `cmd/duetty-eval/correct.go`
- Test: `cmd/duetty-eval/correct_test.go`

**Interfaces:**
- Consumes: `translate.Client` (Tasks 3–4), `stt.Client.SetHints` (Task 5), `app.resolveHints`. Export it as `app.ResolveHints` if `cmd` needs it, or reuse `ctxfile` plus `WhisperHints` directly.
- Produces flags:
  - `-correct`: after resolving each clip, translate the heard text with `Correct:true` and `Correct:false`, both into the pair's other language.
  - `-context PATH`: a context file. Its body is given to the translator, and its hints are set on the stt client (generated if absent) so the language and noise suites run with hints.
- Produces: `func sameWords(a, b string) bool` (case-folded, punctuation stripped, whitespace collapsed).

**Output for `-correct`:** one line per clip: `id`, heard, corrected, and a `REPHRASE` tag when `sameWords(heard, ref)` but not `sameWords(heard, corrected)`. Then a summary: `rephrased N/M (clips heard correctly)` and `latency median plain X ms, correct Y ms`. Transcripts go to stdout only, as today.

- [ ] **Step 1: Write failing test**: `TestSameWords`. `("Bonjour, Camille !","bonjour camille")` → true. `("we ship friday","we ship Monday")` → false. `("今日は。","今日は")` → true.
- [ ] **Step 2: Run** `go test ./cmd/duetty-eval/`. Expected: FAIL.
- [ ] **Step 3: Implement** `sameWords`, the two flags and the report.
- [ ] **Step 4: Run** `go test ./cmd/duetty-eval/` and `go build ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(eval): measure correction, latency and Whisper hints`

---

### Task 10: Measure and set defaults

Needs `DUETTY_API_KEY` for Scaleway and the offline setup from `docs/offline.md`. If either isn't available, stop and ask the user to run the commands below and paste the output.

- [ ] **Step 1: Fetch clips** as in `eval/README.md` (`scripts/fetch-tatoeba-clips.sh fr,en,es,ja 10 eval/clips`, plus the words set).
- [ ] **Step 2: Correction on Scaleway:** `./duetty-eval -config my.toml -clips eval/clips/meta.json -correct`. Record rephrased N/M and both latency medians.
- [ ] **Step 3: Correction offline:** the same run with the offline config. Record the same numbers.
- [ ] **Step 4: Hints harmless:** `./duetty-eval ... -clips ... -words ... -noise eval/noise -context internal/ctxfile/testdata/example.md` on Scaleway (and speaches if available). Pass = language pick still 20/20 per pair, and noise and words results equal to the table in `eval/README.md`.
  - If the noise suite regresses: in `stt.Client.Transcribe`, set `Result.Text = ""` (so it's dropped as noise) when the normalised result has ≥ 5 words and is a substring of the normalised hint sent. Add the test `TestHintEchoDropped` to `internal/stt/client_test.go`, then re-run.
  - If it still regresses, the hint fails.
- [ ] **Step 5: Caching:** note whether `CachedTokens` was ever non-zero on Scaleway (add a temporary print or read the log). This is information only.
- [ ] **Step 6: Decide defaults** (spec rule: a failed part ships off by default):
  - Correction fails if any clip is `REPHRASE` for a model, or if the median latency goes up by more than 300 ms (my threshold, recorded with the numbers). If it fails on Scaleway, set `Correct` default false. If it fails only offline, keep the default and say so in `docs/offline.md` (Task 11).
  - If the hint fails, set `WhisperHint` default false.
  - Update `Defaults()`, `config.example.toml` and the Task 2 test accordingly.
- [ ] **Step 7: Record results** in `eval/README.md` in a new "Context, correction and hints" section, in the existing table style.
- [ ] **Step 8: Commit** `eval: measure correction and hints; set defaults`

---

### Task 11: User documentation (last)

**Files:**
- Create: `docs/context.md`, `docs/context.example.md` (the spec appendix example, verbatim), `docs/context-research-prompt.md`
- Modify: `README.md` (feature bullet + short "Context file" section linking `docs/context.md`), `config.example.toml` (final wording of the Context section), `docs/offline.md` (only if Task 10 found a difference offline)

**Content:**
- `docs/context.md`:
  - the format: the sections from the spec, with only `Whisper hint` structured;
  - what each switch does;
  - the cost note (the file is sent with every translation, and prefix caching helps);
  - `-print-whisper-hint` for tuning;
  - the manual hint check: say the names from your file, with and without `-whisper-hint=false`, and compare;
  - how to use the research prompt.
- `docs/context-research-prompt.md`: a prompt for any web-browsing agent.
  - Inputs: the user's keywords, people and brands, plus the two languages.
  - Asks for: correct spellings, one-line descriptions, how each term is rendered in each language, a `Whisper hint` section per language (≤ 150 words, natural sentences, hardest words first), no sources in the output, and output in exactly the shape of `docs/context.example.md`.
- Follow the README's existing plain style.

- [ ] **Step 1: Write** the files.
- [ ] **Step 2: Check** that `go test ./internal/config/` still passes (example config) and that every example command in the docs runs (`-print-whisper-hint` with `docs/context.example.md` prints both hints).
- [ ] **Step 3: Commit** `docs: context file guide, example and research prompt`
