# Research prompt for a context file

Paste the text below into an agent that can browse the web. Replace the
parts in angle brackets. Save its answer as a `.md` file and read it before
use.

---

I am preparing a live translation between <language 1> and <language 2>.
I need a context file that helps a transcription and translation tool get
names and terms right.

About the conversation: <who meets, about what, which company or field>

Keywords and terms: <list>

People: <names, and roles if you know them>

Brands and products: <list>

Please search the web for each item and find:

- the correct spelling, with accents and capitals;
- a one-line description;
- how it is rendered in <language 1> and in <language 2>: translated,
  or kept as is. Note common spoken forms, for example how an acronym is
  pronounced.

Then write the answer in exactly this shape, and nothing else (no intro, no
sources, no links):

    # Setting

    One or two paragraphs about the conversation.

    ## People

    - Name — role. Spelling or pronunciation note.

    ## Brands and products

    - Name — what it is. Translated or not.

    ## Terms

    - term — how it is rendered in each language.

    ## Whisper hint

    ### <language 1 code>

    One or two natural sentences.

    ### <language 2 code>

    One or two natural sentences.

Rules for the Whisper hint section:

- One `###` heading per language, using the Whisper language code (`fr`,
  `en`...).
- At most 150 words per language.
- Write natural sentences, not a list of words.
- Put the hardest words first: names, brands, unusual spellings.
- Write each hint in its own language.

If you are not sure about a spelling, leave the item out instead of guessing.
