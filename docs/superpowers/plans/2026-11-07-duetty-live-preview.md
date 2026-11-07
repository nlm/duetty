# Live preview, sentence commits and cost counter — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Optional live preview (provisional text + translation in both panes while someone speaks, finished sentences committed early) and an always-on session cost counter.

**Architecture:** The VAD listener streams every byte of the utterance it is capturing to a new `internal/live` previewer, which re-transcribes the uncommitted part of the recording on a paced schedule, commits finished sentences from Whisper's segments, and drives new "live" entries in the conversation log. A new `internal/usage` meter is fed by the STT and translation clients from the API `usage` fields of every call and rendered in the status bar.

**Tech Stack:** Go, tview/tcell, BurntSushi/toml, OpenAI-compatible Whisper (`verbose_json`) and chat completions.

**Spec:** `docs/superpowers/specs/2026-11-07-duetty-live-preview-design.md` (extends `docs/superpowers/specs/2026-11-07-duetty-design.md`)

## Global Constraints

- `live_preview` (flag `-live-preview`) default **false**; when false, behavior is exactly today's (one transcription per turn) and the cost counter is still shown.
- `preview_interval_ms` default `1000`, config file only (no flag).
- `price_audio_per_min`, `price_in_per_mtok`, `price_out_per_mtok` have **no defaults**; `price_currency` default `""`. A price estimate is shown only when all three prices are set.
- Refresh pacing: send again only when new audio ≥ `preview_interval_ms` **and** ≥ a quarter of the live recording's length; at most one preview transcription in flight per turn.
- Language lock after ≥ 3 s of live recording; every commit unlocks.
- Commit up to the last segment that ends in `. ? ! … 。 ？ ！` (ignoring trailing quotes/brackets) and is followed by another segment; never the newest segment.
- Every billed call counts (preview refreshes, forced passes, discarded/cancelled translations whose response arrived). A response without `usage` counts nothing.
- Nothing persisted; the API key never logged; the log file records timings and counts only, never text.
- Provisional style: gray italic (`[gray::i]`) for both the original and its live translation; committed rows look exactly as today.
- `go test -race ./...` and `go vet ./...` must pass after every task.
- Not in this plan (ruling): the optional eval replay of joined clips through the live path; the Task 8 fakes cover the commit/trim logic, and the real-audio check is the user's smoke test.

## Review Focus

1. **Turn hand-off race:** the listener can start a new utterance before the delivery goroutine hands the previous one to the previewer. Expected: each delivered utterance finishes its own live entry, never the newer one. (Task 8 test `TestEndPairsWithOldestTurn`.)
2. **Escape while the final pass is running:** expected: the provisional line disappears and the late result is ignored. (Task 5 test `TestCancelPendingDropsFinishingLive`.)
3. **A refresh result arriving after a commit, End or Abort:** expected: ignored, no duplicated or resurrected text. (Task 8 test `TestStaleRefreshIgnored`.)
4. **A segment end time beyond the snapshot** (Whisper rounding) or a commit at the very end: expected: trim clamps to the snapshot, no panic, no lost audio after it. (Task 8 test `TestCommitTrimClamped`.)
5. **Long monologue that never splits:** expected: refreshes slow down (≤ about 4× real-time audio billed), and the whole thing is committed at end of turn. (Task 8 test `TestPacingSlowsForLongRecording`.)

---

### Task 1: Usage meter and formatting

**Files:**
- Create: `internal/usage/usage.go`, `internal/usage/usage_test.go`

**Interfaces:**
- Produces:
  - `type Totals struct { AudioSeconds float64; InTokens, OutTokens int64 }`
  - `type Meter struct` (mutex inside); `func NewMeter(onChange func()) *Meter`; methods `AddAudio(sec float64)`, `AddTokens(in, out int64)`, `Totals() Totals`. All methods are safe on a nil `*Meter` (no-op / zero Totals). `onChange` (may be nil) is called after each Add, outside the lock.
  - `type Prices struct { AudioPerMin, InPerMTok, OutPerMTok float64; Currency string }`
  - `func Format(t Totals, p *Prices) string` (p nil = no price).

- [ ] **Step 1: Write the failing tests** (`usage_test.go`)

```go
func TestFormatNoPrice(t *testing.T) {
	got := Format(Totals{AudioSeconds: 250, InTokens: 15000, OutTokens: 3300}, nil)
	if got != "18.3k tok · 4m10s audio" { t.Fatal(got) }
}
func TestFormatWithPrice(t *testing.T) {
	p := &Prices{AudioPerMin: 0.003, InPerMTok: 0.15, OutPerMTok: 0.35, Currency: "€"}
	got := Format(Totals{AudioSeconds: 250, InTokens: 15000, OutTokens: 3300}, p)
	if got != "≈ €0.016 · 18.3k tok · 4m10s audio" { t.Fatal(got) }
}
func TestFormatSmallAndLarge(t *testing.T) {
	cases := map[Totals]string{
		{}: "0 tok · 0s audio",
		{AudioSeconds: 42.4, InTokens: 600, OutTokens: 250}: "850 tok · 42s audio",
		{AudioSeconds: 3725, InTokens: 1_500_000, OutTokens: 250_000}: "1.75M tok · 1h02m audio",
	}
	for in, want := range cases { if got := Format(in, nil); got != want { t.Errorf("%v: %q want %q", in, got, want) } }
}
func TestMeterAccumulatesAndNotifies(t *testing.T) {
	n := 0
	m := NewMeter(func() { n++ })
	m.AddAudio(3); m.AddTokens(23, 9); m.AddTokens(1, 1)
	if got := m.Totals(); got != (Totals{AudioSeconds: 3, InTokens: 24, OutTokens: 10}) || n != 3 { t.Fatal(got, n) }
	var nilM *Meter
	nilM.AddAudio(1); nilM.AddTokens(1, 1)
	if nilM.Totals() != (Totals{}) { t.Fatal("nil meter") }
}
```

- [ ] **Step 2: Run** `go test ./internal/usage` — Expected: FAIL (package does not exist).
- [ ] **Step 3: Implement** `usage.go`. Format rules: tokens = In+Out; `< 1000` → `"%d"`, `< 1e6` → `"%.1fk"`, else `"%.2fM"`. Audio rounded to whole seconds: `< 60` → `"%ds"`, `< 3600` → `"%dm%02ds"`, else `"%dh%02dm"`. Cost = sec/60·AudioPerMin + in/1e6·InPerMTok + out/1e6·OutPerMTok, printed `"≈ %s%.3f · "` with Currency.
- [ ] **Step 4: Run** `go test -race ./internal/usage` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(usage): session usage meter and status formatting`

---

### Task 2: Clients report usage

**Files:**
- Modify: `internal/stt/client.go`, `internal/translate/translate.go`
- Test: `internal/stt/client_test.go`, `internal/translate/translate_test.go` (existing httptest-based tests; extend)

**Interfaces:**
- Consumes: `usage.Meter` (Task 1).
- Produces: `func (c *stt.Client) SetMeter(m *usage.Meter)`, `func (c *translate.Client) SetMeter(m *usage.Meter)`. Signatures of `NewClient` unchanged.

- [ ] **Step 1: Write failing tests**
  - `TestTranscribeCountsAudioSeconds`: server returns `{"text":"hi","language":"en","usage":{"type":"duration","seconds":3}}`; after `Transcribe`, `m.Totals().AudioSeconds == 3`.
  - `TestTranscribeWithoutUsageCountsNothing`: no `usage` field → `AudioSeconds == 0`; non-2xx response → 0.
  - `TestTranslateCountsTokens`: server returns `"usage":{"prompt_tokens":23,"completion_tokens":9}` → `InTokens==23, OutTokens==9`.
  - `TestTranslateCountsWithoutChoices`: a 2xx response with `"choices":[]` and usage still counts its tokens (the call was billed) while `Translate` returns its usual error.
- [ ] **Step 2: Run** `go test ./internal/stt ./internal/translate` — Expected: FAIL (`SetMeter` undefined).
- [ ] **Step 3: Implement.** Add `Usage struct{ Type string; Seconds float64 }` to the stt response (count only when `Type == "duration"`), and `Usage struct{ PromptTokens, CompletionTokens int64 }` to the translate response. Count right after a successful JSON parse of a 2xx response, before any further checks.
- [ ] **Step 4: Run** `go test -race ./internal/stt ./internal/translate` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(stt,translate): count audio seconds and tokens from API usage`

---

### Task 3: Whisper segments and locked-language resolve

**Files:**
- Modify: `internal/stt/client.go`, `internal/stt/resolve.go`
- Test: `internal/stt/client_test.go`, `internal/stt/resolve_test.go`

**Interfaces:**
- Produces:
  - `type Segment struct { Start, End float64; Text string }` (seconds, on the uploaded audio's timeline)
  - `Result.Segments []Segment`; `Resolved{ Lang, Text string; Segments []Segment }` — `check` copies the chosen pass's segments.
  - `func (r *Resolver) ResolveLocked(ctx context.Context, wav16k []byte, lang string) (Resolved, error)` — `lang == ""` → exactly `Resolve`; otherwise one Whisper call forced to `lang`, noise floor applied (`ErrNoise`), result labelled `lang`.

- [ ] **Step 1: Write failing tests**
  - `TestTranscribeParsesSegments`: two segments `{"start":0,"end":2.6,"text":" Tom is angry."}`, `{"start":3.2,"end":4.9,"text":" Don't."}` → `Segments == []Segment{{0, 2.6, "Tom is angry."}, {3.2, 4.9, "Don't."}}` (text trimmed).
  - `TestResolveLockedSinglePass`: fake transcriber records calls; `ResolveLocked(ctx, wav, "fr")` makes exactly one call with language `"fr"`, returns `Lang "fr"` and the segments.
  - `TestResolveLockedNoise`: forced result with `AvgLogprob -2` → `ErrNoise`.
  - `TestResolveLockedEmptyLangIsResolve`: `ResolveLocked(ctx, wav, "")` makes the same calls as `Resolve` (first call auto, language `""`).
- [ ] **Step 2: Run** `go test ./internal/stt` — Expected: FAIL.
- [ ] **Step 3: Implement** (segment fields `start`, `end`, `text` added to the existing segment struct).
- [ ] **Step 4: Run** `go test -race ./internal/stt` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(stt): segments in results and ResolveLocked`

---

### Task 4: Configuration

**Files:**
- Modify: `internal/config/config.go`, `config.example.toml`, `README.md`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `usage.Prices` (Task 1).
- Produces: `Config.LivePreview bool` (`live_preview`, flag `-live-preview`), `Config.PreviewIntervalMS float64` (`preview_interval_ms`, default 1000), `Config.PriceAudioPerMin, PriceInPerMTok, PriceOutPerMTok *float64` (`price_audio_per_min`, `price_in_per_mtok`, `price_out_per_mtok`), `Config.PriceCurrency string` (`price_currency`); `func (c Config) Prices() *usage.Prices` — nil unless all three price pointers are set.

- [ ] **Step 1: Write failing tests**
  - `TestLivePreviewDefaultsOff`: `Defaults().LivePreview == false`, `Defaults().PreviewIntervalMS == 1000`, all price pointers nil, `Defaults().Prices() == nil`.
  - `TestLivePreviewFlag`: `Load([]string{"-live-preview", "-base-url", "https://x"}, …)` → `LivePreview == true`.
  - `TestPricesFromFile`: TOML with all three prices and `price_currency = "€"` → `Prices()` equals `&usage.Prices{0.003, 0.15, 0.35, "€"}`; TOML with only two prices → `Prices() == nil`.
  - `TestValidatePreviewAndPrices`: `preview_interval_ms = 50` → error mentioning `preview_interval_ms` (minimum 100); a negative or NaN price → error naming the key.
- [ ] **Step 2: Run** `go test ./internal/config` — Expected: FAIL.
- [ ] **Step 3: Implement**; add the new keys to `config.example.toml` (live_preview commented with its default; prices commented out, with the Scaleway figures in a comment marked "for reference, not defaults") and a short README section "Live preview and cost".
- [ ] **Step 4: Run** `go test -race ./internal/config` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(config): live_preview, preview_interval_ms and optional prices`

---

### Task 5: Live entries in the conversation log

**Files:**
- Modify: `internal/convo/convo.go`
- Test: `internal/convo/live_test.go` (new)

**Interfaces:**
- Consumes: `stt.Resolved.Segments`, `ResolveLocked` (Task 3).
- Produces:
  - `const Live Status` (added after `Failed`; `String()` = `"live"`); `Row.Stale bool` (Live only: `Translation` was made for older text).
  - `convo.Resolver` gains `ResolveLocked(ctx context.Context, wav16k []byte, lang string) (stt.Resolved, error)` (satisfied by `*stt.Resolver`; update test fakes).
  - `type LiveID uint64`
  - `func (l *Log) StartLive() LiveID` — new Live entry, Seq assigned now; hidden from `Rows()` while its `Original` is empty.
  - `func (l *Log) UpdateLive(id LiveID, lang, text string)` — sets provisional `Lang`/`Original`; ignored once finishing/gone.
  - `func (l *Log) CommitLive(id LiveID, lang, text string)` — inserts a normal row (`Original`, then translated as today, with history) at the live entry's position taking its Seq; the live entry gets the next Seq, moves to the end of the log, and its `Original`/`Translation`/`Stale` are cleared.
  - `func (l *Log) FinishLive(id LiveID, wav16k []byte, lang string)` — runs `ResolveLocked(ctx, wav16k, lang)` (RequestTimeout); while running the entry stays Live with its provisional text and counts in `InFlight`; result: success → becomes `Original` with the final text and is translated; `ErrNoise` → removed; error → `Failed` (same message format as today).
  - `func (l *Log) DropLive(id LiveID)` — removes the entry, cancels its requests.
  - `CancelPending` also drops every Live entry (finishing or not).

Live translation (inside the Log): whenever `UpdateLive` sets text different from the last text sent for translation, translate it (same `startTranslation` direction rule and history as a normal row). At most one live translation in flight per entry; a newer text waits as the single queued text (latest wins). A result is applied only while the entry is still Live and not finishing: `Translation = out`, `Stale = (text it translated != current Original)`. Errors leave the previous translation.

- [ ] **Step 1: Write failing tests** (fakes: resolver and translator with controllable, blocking results)
  - `TestLiveHiddenUntilText`: `StartLive()` → `Rows()` empty; `UpdateLive(id,"fr","Bonjour")` → one row `{Status: Live, Lang:"fr", Original:"Bonjour"}`.
  - `TestLiveTranslationLatestWins`: block the translator; `UpdateLive` "a", "a b", "a b c" → translator called with "a", then (after release) "a b c" only; final row `Translation` = translation of "a b c", `Stale == false`.
  - `TestLiveTranslationStaleFlag`: translation of "a" returns after `UpdateLive("a b")` → row shows `Translation(a)` with `Stale == true`.
  - `TestCommitLiveOrdering`: rows `[typed T1]`, live L started after T1, then `SubmitText(T2)`, then `CommitLive(L,"fr","Phrase un.")` → `Rows()` order: T1, "Phrase un." (Original/Done), T2, then the live entry after `UpdateLive`; the committed row's history contains T1 only.
  - `TestFinishLiveBecomesRow`: `FinishLive` with resolver result `{fr, "Fin."}` → while blocked, row is Live showing old provisional text and `InFlight()==1`; after release → `Original` "Fin." then `Done`.
  - `TestFinishLiveNoiseRemoves` / `TestFinishLiveErrorFails`.
  - `TestFinishLivePassesLockedLang`: resolver fake records `lang` argument `"en"`.
  - `TestCancelPendingDropsFinishingLive`: block `FinishLive`'s resolve; `CancelPending()` → entry gone from `Rows()`; release resolver → still gone.
  - `TestDropLive`: entry removed; a late live translation result is ignored.
- [ ] **Step 2: Run** `go test ./internal/convo` — Expected: FAIL.
- [ ] **Step 3: Implement.** Keep a `map[LiveID]*entry`; live-translation state (`inFlight bool`, `queued string`, `sentText string`, `finishing bool`) lives on `entry`.
- [ ] **Step 4: Run** `go test -race -count=3 ./internal/convo` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(convo): live entries with latest-wins live translation`

---

### Task 6: UI — live rows and usage in the status bar

**Files:**
- Modify: `internal/ui/layout.go`, `internal/ui/app.go`
- Test: `internal/ui/layout_test.go`, `internal/ui/app_test.go`

**Interfaces:**
- Consumes: `convo.Live`, `Row.Stale` (Task 5).
- Produces: `func (a *ui.App) SetUsage(text string)` (non-blocking, any goroutine, same pattern as `SetNotice`); shown at the end of the status line after `" · "`, omitted when `""`.

Layout: a Live row joins/creates groups like any row. Source seg style `liveStyle = "[gray::i]"` with its `Original`; other side: `Translation == ""` → `{liveStyle, "…"}`; else `{liveStyle, Translation}` plus `{liveStyle, "…"}` when `Stale`.

- [ ] **Step 1: Write failing tests**
  - `TestLayoutLiveRow`: rows `[Done fr "Bonjour."/"Hello.", Live fr "Je voudrais"/"I would like" Stale]` → left line 0 plain text `"» Bonjour. Je voudrais"`, contains `"[gray::i]Je voudrais"`; right contains `"[gray::i]I would like"` followed by `"…"`.
  - `TestLayoutLiveNoTranslationYet`: Live en row, empty Translation → left (fr side) contains `"[gray::i]…"`.
  - `TestStatusShowsUsage`: `SetUsage("850 tok · 42s audio")` → screen dump status line ends with that text; `SetUsage("")` → not shown.
- [ ] **Step 2: Run** `go test ./internal/ui` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./internal/ui` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): provisional live rows and usage counter`

---

### Task 7: Listener streams utterance audio and can drop the current capture

**Files:**
- Modify: `internal/vad/listener.go`
- Test: `internal/vad/listener_test.go`

**Interfaces:**
- Produces:
  - `Listener.OnAudio func(data []byte, start bool)` — called on Run's goroutine, outside `l.mu`, for every byte range appended to the utterance being captured, in order; `start == true` on the first call of an utterance (data = pre-buffer + onset chunk). Invariant: the concatenation of calls from a `start` up to the next `start` equals the delivered utterance, or the discarded one (signalled by `OnSpeechAbort`). Called before that chunk's delivery. Callee must not retain `data`.
  - `func (l *Listener) DropCurrent() bool` — discards an in-progress capture or turn-judge hold (detector reset, hold generation bumped, buffer cleared), fires `OnSpeechAbort`, returns true; returns false (and does nothing) when idle. Listening continues: later speech starts a new utterance with `start == true`.

- [ ] **Step 1: Write failing tests** (existing fake detector/judge helpers)
  - `TestOnAudioConcatEqualsUtterance`: plain endpointing, two utterances → per-utterance concatenation of `OnAudio` data equals each value received on `Utterances()`; `start` true exactly twice.
  - `TestOnAudioDuringHold`: with a turn judge that says "wait" then "done", chunks fed during the hold are reported and the concatenation still equals the delivered utterance.
  - `TestDropCurrentDiscards`: during speech `DropCurrent()` → true, `OnSpeechAbort` called once, nothing delivered; further speech → new `start == true` and a delivered utterance equal to the post-drop audio only.
  - `TestDropCurrentIdle`: returns false, no callback.
  - `TestDropCurrentDuringHold`: same as discard, and a judge answer arriving afterwards delivers nothing.
- [ ] **Step 2: Run** `go test ./internal/vad` — Expected: FAIL.
- [ ] **Step 3: Implement.** Record the appended range under the lock in `handleChunk`, call `OnAudio` after unlock and before `deliver`.
- [ ] **Step 4: Run** `go test -race -count=3 ./internal/vad` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(vad): OnAudio stream and DropCurrent`

---

### Task 8: The live previewer

**Files:**
- Create: `internal/live/live.go`, `internal/live/rules.go`, `internal/live/live_test.go`, `internal/live/rules_test.go`

**Interfaces:**
- Consumes: `stt.Resolved`, `stt.Segment` (Task 3); `convo.LiveID` and the Live methods (Task 5).
- Produces:
  - `func CommitCount(segs []stt.Segment) int` — number of leading segments to commit (rule in Global Constraints).
  - `func Due(newMS, liveMS, intervalMS float64) bool` — `newMS >= intervalMS && newMS >= liveMS/4`.
  - `type Resolver interface { ResolveLocked(ctx context.Context, wav16k []byte, lang string) (stt.Resolved, error) }`
  - `type Sink interface { StartLive() convo.LiveID; UpdateLive(convo.LiveID, string, string); CommitLive(convo.LiveID, string, string); DropLive(convo.LiveID) }` (`*convo.Log` satisfies it)
  - `type Config struct { IntervalMS, MinMS float64; LockAfter time.Duration; Timeout time.Duration; Encode func(pcm48k []byte) ([]byte, error) }` (app passes `IntervalMS = preview_interval_ms`, `MinMS = vad_min_speech_ms`, `LockAfter = 3*time.Second`, `Timeout = convo.RequestTimeout`, `Encode = stt.PCM48kToWAV16k`)
  - `func New(cfg Config, r Resolver, s Sink) *Previewer`
  - `func (p *Previewer) Audio(data []byte, start bool)` — wire to `Listener.OnAudio`; never blocks on the network.
  - `func (p *Previewer) Abort()` — drops the newest turn if it is still receiving audio (`DropLive`), ignores its in-flight results.
  - `func (p *Previewer) End(utterance []byte) (rest []byte, id convo.LiveID, lang string)` — pairs with the **oldest** turn not yet ended; returns the uncommitted part of `utterance` (bytes after the committed offset) and the turn's locked language (`""` if unlocked). Later results for that turn are ignored.
  - `func (p *Previewer) Close()` — waits for in-flight refreshes (bounded by Timeout).

Behavior (per turn; audio is 48 kHz mono s16le, 96 000 bytes/s):
- `start == true` creates a turn (`StartLive`) and seals the previous one (no more refreshes for it).
- After each `Audio`, for the newest unsealed turn with no refresh in flight: if live recording ≥ `MinMS` and `Due(new since last send, live length, IntervalMS)` → snapshot the live recording `[base, len)`, `Encode`, `ResolveLocked(ctx, wav, lockedLang)` on a goroutine.
- On result (ignored if the turn was sealed-and-ended, aborted, or `base` moved since the snapshot): `ErrNoise`/error → nothing. Else `n := CommitCount(segs)`; if `n > 0`: `CommitLive(id, res.Lang, join(segs[:n]))`, `base += clamp(bytesAt(segs[n-1].End), 0, snapshotLen)` (even byte count), unlock language, then `UpdateLive(id, shownLang, join(segs[n:]))`. If `n == 0`: `UpdateLive(id, shownLang, res.Text)`. Join texts with a space unless both sides are wide (CJK) runes.
- Language: `shownLang` is the first result's `Lang` after the turn start or a commit; if unlocked and the snapshot was ≥ `LockAfter`, lock to `res.Lang` and set `shownLang` to it.

- [ ] **Step 1: Write failing tests**
  - `rules_test.go`: `CommitCount` — `["Bonjour.", "Je voudrais"]` → 1; `["Bonjour.", "Ça va ?", "Et"]` → 2; `["We must take", "measures"]` → 0; `["« Va chercher Tom. »", "J'ai"]` → 1; `["座りませんか？", "駅は"]` → 1; single segment `["Fin."]` → 0; `[]` → 0. `Due` — `(1000, 4000, 1000)` true; `(1000, 20000, 1000)` false; `(5000, 20000, 1000)` true; `(900, 1000, 1000)` false.
  - `live_test.go` (fake resolver returning scripted `Resolved` and blocking on demand; fake sink recording calls; audio as zero bytes):
    - `TestRefreshAndCommit`: feed 2 s → one refresh; result segs `[{0,1.2,"Bonjour."},{1.5,2.0,"Je"}]` → sink sees `CommitLive(id,"fr","Bonjour.")` then `UpdateLive(id,"fr","Je")`; `End(all)` returns `rest` == bytes from 1.2 s on.
    - `TestOneRefreshInFlight`: blocked resolver, feed 5 s → exactly one call until released.
    - `TestPacingSlowsForLongRecording`: 20 s live recording, no commits → refresh calls ≤ 8 over the 20 s (vs 20 at 1 s fixed).
    - `TestLanguageLocksAfter3s`: first refresh at 1 s → `lang ""`; refresh after 3 s of recording → later calls pass `"fr"`; after a commit the next call passes `""` again.
    - `TestStaleRefreshIgnored`: refresh in flight, `End` called, then result released → no sink calls after End's.
    - `TestCommitTrimClamped`: segment end 9.9 s on a 2 s snapshot → `base` = snapshot end; `End` returns only the audio after it.
    - `TestEndPairsWithOldestTurn`: turn A audio, turn B `start`, then `End(A's bytes)` returns A's id and A's rest; B keeps refreshing.
    - `TestAbortDropsNewest`: `Abort()` → `DropLive(id)`; a late result for it → no further sink calls.
- [ ] **Step 2: Run** `go test ./internal/live` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=3 ./internal/live` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(live): paced re-transcription with sentence commits`

---

### Task 9: Wire it into the app

**Files:**
- Modify: `internal/app/run.go`
- Test: `internal/app/run_test.go`

**Interfaces:**
- Consumes: everything above. `app.UI` interface gains `SetUsage(string)`; test fakes updated.

Wiring:
- Always: `meter := usage.NewMeter(onUsage)`; real clients get `SetMeter(meter)`; `onUsage` serializes `u.SetUsage(usage.Format(meter.Totals(), cfg.Prices()))` behind a mutex (same reason as `rowPusher`); push once before `u.Run()`. `Deps` gains `Meter *usage.Meter` (nil → new one) so tests can drive the counter.
- `cfg.LivePreview && audio != nil`: build `live.New(...)`; `lst.OnAudio = prev.Audio`; `OnSpeechAbort` also calls `prev.Abort()`; worker: `rest, id, lang := prev.End(pcm)`, trim `rest` exactly as `processUtterance` does today (refactor it to return the WAV or nil), then `cl.FinishLive(id, wav, lang)` or `cl.DropLive(id)` when nothing is left; `OnCancel` also calls `lst.DropCurrent()`; `prev.Close()` on shutdown after the listener stops. Log only durations/counts (`"live commit: %d segments"`, `"live refresh: %s audio"`), never text.
- Live preview off: unchanged path (`cl.SubmitAudio`).

- [ ] **Step 1: Write failing tests** (existing fake Audio/UI/resolver/translator harness)
  - `TestUsageShownInStatus`: fake UI records `SetUsage`; `Deps.Meter.AddTokens(600, 250)` → last recorded text `"850 tok · 0s audio"`; with all three prices in cfg → text starts with `"≈ "`.
  - `TestLivePreviewOffUsesSubmitAudio`: default cfg, one utterance → resolver's `ResolveLocked` never called with a live snapshot; one row as today.
  - `TestLivePreviewCommitsBeforeEndOfTurn`: `LivePreview: true`, fake detector producing a 4 s utterance, resolver scripted to return two segments on the refresh → a committed row (`Original`/`Done`) is visible **before** the fake detector ends the turn; after the end, the final row comes from `FinishLive` with the remaining audio.
  - `TestEscapeDropsLiveTurn`: during the fake utterance, call the UI's cancel callback → live row disappears and nothing is delivered for that turn.
- [ ] **Step 2: Run** `go test ./internal/app` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./... && go vet ./... && go build -o poly ./cmd/poly && ./poly -h | grep -- -live-preview` — Expected: all PASS, flag listed.
- [ ] **Step 5: Commit** `feat(app): wire live preview and cost counter`
