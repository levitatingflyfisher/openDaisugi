# The foreman answers aloud

Speak to the foreman with the mic, and it answers aloud. Type to it, and it
answers in text. One rule, on the desk and on the phone.

## What you get with no setup

Nothing to install. The page speaks with your browser's own voice
(`speechSynthesis`), sentence by sentence.

- A reply is spoken when the message it answers came by voice: the mic put
  words in the tell bar, and you sent them. Words you edit after the mic
  still count as voice. The mic in a pane's own prompt box does not: those
  words are for that agent. A reply to a typed message is not spoken.
- The mark covers the replies to that one message. The next message to the
  foreman, from you or anyone, ends it.
- Settings has **Read replies aloud**. Turn it on to hear every reply. The
  choice stays in that browser.
- Each reply in the chat has a **Listen** button. A tap reads that reply.
- A mic press stops the speech, and so does the **Stop** button that shows
  while it speaks.
- Sound is unlocked by your first tap or mic press. The browser allows no
  sound before that.

The browser's voice stops when the screen locks. The page says so once. For
a better voice, use a speech server.

## A better voice: one URL

Set one address in `coppice.toml`:

```toml
[voice]
speak_url = "http://127.0.0.1:8880"   # a speech server; see below
speak_model = "kokoro"                 # optional; sent to that server
speak_voice = "af_heart"               # optional; sent to that server
speak_profile = "tts"                  # optional; an agent profile for a key
```

Then start the coppice server again. The address names any server with an
OpenAI-style `POST /v1/audio/speech`. It may be:

- a program on this box,
- another box on your tailnet (the Machines panel lists it as `speech`),
- a cloud service you choose.

Each reply's text is sent to that address, so choose a server you trust with
what the foreman says. Settings shows the address, the model and the voice,
and says so; it cannot edit them.

The address takes no user name, password or query. A key never lives in
`coppice.toml`. If the server needs a key, put it in an operator agent
profile (`daisugi agent-profiles`, see [agent profiles](agent-profiles.md))
and name that profile in `speak_profile`. The coppice server reads the key
from the profile when it asks, sends it as the bearer token, and shows it
nowhere. A key goes only over `https`, or over plain `http` to this box or
the tailnet (`127.0.0.1`, `localhost`, `100.64.0.0/10`,
`fd7a:115c:a1e0::/48`, or a `*.ts.net` name). Over plain `http` to any other
host, the request is refused with one line that says so.

How it works: the page asks the coppice server for one sentence at a time
(`speech.say`). The server asks the speech server, with 10 seconds and 2 MiB
of audio for each sentence, using no proxy and following no redirect. The
page gets the audio as a binary frame on its socket and plays the sentences
in order with WebAudio. It asks for the next sentence while one plays, so
the gaps stay short.

If the speech server fails, the page speaks the rest of that reply with the
browser's voice, and one line says why and what to fix, for example: `The
speech server at gpu:8880 did not answer. Check that it runs and that
speak_url in coppice.toml names it. This reply is spoken with the browser's
voice.`

WebAudio plays while the page is open. Playback may stop when a phone locks
its screen. The page keeps no audio and no text of the replies.

## Run Kokoro on this box or on a GPU box

Kokoro-82M is a small text-to-speech model. Its weights are Apache-2.0
(checked 2026-10-15). [Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI)
serves it with `POST /v1/audio/speech`. Read its `LICENSE` for the project's
own licence. Its text-to-phoneme step uses espeak-ng, which is GPL-3.0. That
runs inside the separate speech server only, never inside our binaries.
coppice ships none of it and links none of it.

Follow its README to start the server (a container image for CPU or GPU is
the usual way). It listens on port 8880. Then:

```toml
[voice]
speak_url = "http://127.0.0.1:8880"    # or http://gpu-box:8880 on the tailnet
```

The defaults for `speak_model` and `speak_voice` (`kokoro`, `af_heart`) are
what Kokoro-FastAPI expects. Over a tailnet the address is plain `http`
inside your own network. For a cloud service, use its `https://` address,
its model and voice names, and a `speak_profile` for its key.

## Why not Piper

Piper is a good engine, but its current releases are GPL-3.0 (checked
2026-10-15), and each voice carries its own licence. coppice does not bundle
an engine, and nothing GPL goes in its binaries, so Piper is not the
default. A Piper server that speaks the same `/v1/audio/speech` API works
with `speak_url` like any other, and its licence is then yours to read.

## The words of the protocol

`speech.status` tells the page how replies are spoken. `speech.say` asks for
one sentence. The web socket answers it with a binary frame, and only to the
socket that asked. See `harness/coppice/PROTOCOL.md`, "Speech out".
