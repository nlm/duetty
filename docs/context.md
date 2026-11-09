# Context file

A context file tells duetty who is talking and about what. duetty uses it to
fix words Whisper misheard (names, brands, jargon) and to translate them the
way you want.

```sh
duetty -languages fr,en -context docs/context.example.md
```

Or in `config.toml`: `context_file = "docs/context.example.md"`.

The path must exist, or duetty stops at startup.

## Format

Plain Markdown. Write what helps a translator who has never heard of your
meeting. [`context.example.md`](context.example.md) shows the shape:

- **Setting**: one or two paragraphs. Who meets, about what, in which field.
- **People**: names, roles, how to spell them.
- **Brands and products**: what never gets translated, what stays in English.
- **Terms**: how a word is rendered in each language.
- **Whisper hint** (optional): see below.

The headings are a suggestion. duetty reads only the `Whisper hint` section
on its own. The rest of the file is sent as written.

## Switches

| Flag | Config key | Default | What it does |
|------|-----------|---------|--------------|
| `-context` | `context_file` | none | The file to use |
| `-correct` | `correct` | `true` | Fix misheard words in the speaker's pane |
| `-mark-corrections` | `mark_corrections` | `true` | Show the changed words in blue |
| `-whisper-hint` | `whisper_hint` | `false` | Send a hint to Whisper (needs `-context`) |
| `-print-whisper-hint` | none | | Print the hints and exit |

### Correction

With `-correct`, the translator first fixes mishearings in the sentence, then
translates it. It never rephrases. Changed words show in blue; use
`-mark-corrections=false` to hide that. Typed text is never corrected.

duetty checks each change the model makes, word by word, and keeps only
fixes that sound like the heard words ("scale way" -> "Scaleway") and small
missing words. A name or a context term is kept only where the heard words
sound like it, so it never appears out of nowhere and a name is never swapped
for one that sounds different ("Tom" stays "Tom" even if the context file
lists a "Dan"). When it drops a change, it keeps the heard words and
translates the sentence again, so the translation matches what is shown.

Correction also works without a context file, but it knows no names then.
`-correct=false` translates the text exactly as heard.

Measured on Scaleway: without a context file, 0 of 102 sentences that were
heard right were changed. With the example context file, 1 of 102 was changed
by one letter ("Piense" -> "Pense"), and no name was swapped. Each sentence
takes about 0.2 s more (median). See
[`eval/README.md`](../eval/README.md).

### Whisper hints

A hint is a short text sent to Whisper with the audio, so it spells your names
right. There is one per language. duetty takes them from the file's
`## Whisper hint` section, with one `### fr`, `### en`... heading per language:

```markdown
## Whisper hint

### fr
Réunion entre Scaleway et Northwind Logistics avec Camille Moreau...

### en
Meeting between Scaleway and Northwind Logistics with Camille Moreau...
```

Hints from your file are used as written, so keep them short: Whisper reads
only about 224 tokens.

Without any hint for your two languages, duetty asks the translation model for
hints at startup (one call) and cuts them to 150 words. If the file has a hint
for only one of the two languages, the other language gets none. Hints are
never sent on Whisper's first pass, which guesses the language.

Hints are **off by default**. In the evaluation a hint made Whisper drop one
badly recorded short clip, so the plain setup is safer. Turn them on with
`-whisper-hint` if Whisper keeps mishearing unusual names. Then check that they
help:

1. Say a few sentences with the names from your file, with `-whisper-hint`.
2. Say the same sentences with `-whisper-hint=false`.
3. Compare the text. Keep the hint only if it is better, and no short word is
   lost.

To see or tune generated hints, run:

```sh
duetty -languages fr,en -context docs/context.example.md -print-whisper-hint
```

It prints a `### fr` and a `### en` section and exits. Paste them into the
file under a `## Whisper hint` heading, edit them, and duetty uses your text as is. When the file already has
the section, it prints that and makes no model call.

## Cost

The whole file is sent with every translation request. Keep it short: duetty
warns at startup above about 2000 tokens. The file comes first in the request,
so a provider with prefix caching can reuse it. Scaleway reported no cached
tokens in our test. Correction asks for the corrected sentence plus the
translation, so output tokens roughly double, live previews included.

## Offline

The context file with speaches and Ollama was not measured.

## Make a file with an agent

[`context-research-prompt.md`](context-research-prompt.md) is a prompt for any
agent that can browse the web. Fill in your keywords, people, brands and the
two languages, paste it in, and save the answer as your context file. Read it
before use: an agent can get a spelling wrong.
