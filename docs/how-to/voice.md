# Record anywhere, land it in a pane

`daisugi voice` turns a recording into text in the pane you choose. You always
see the text before the pane's agent does. Direct send stays off for a pane
until you turn it on.

## Turn it on

The default engine is faster-whisper. Install its extra, create a token,
then start the server. Every request needs the token now, including one
from this same box, so `daisugi voice serve` will not start without it.

```bash
uv add 'opendaisugi[voice]'
coppice web token
daisugi voice serve
```

This binds `127.0.0.1:7477`. It answers `GET /health`, `POST /transcribe`, and
`POST /deliver`. On first use it downloads the `tiny.en` speech model. The
download is about 78 MB. It stays cached after that.

If you use Parakeet instead, see "A second engine: Parakeet" below. You do
not need the faster-whisper extra for that path.

The server starts even with no pane backend running. Preview mode never
needs one. Direct send does: an armed pane that asks for direct send with no
backend reachable gets a clear answer naming the fix, `coppice server
start`, or install tmux.

## Record from a laptop

```bash
daisugi voice ptt w1:p1
```

Tap space to start recording. Tap it again to stop and send. The text
transcribes and previews in pane `w1:p1`. Press `q` to quit. Pass
`--data-dir` if the server you are talking to uses a non-default data
directory.

## From the phone

Open the pane in the coppice web app. Tap Record. The first tap asks the
browser for microphone permission; allow it. Tap Record again to stop. The
text lands in the prompt box. Read it, then tap Prompt or Steer yourself.

This needs `coppice web serve` started with `--voice-url`, pointed at the
address `daisugi voice serve` listens on, for example `--voice-url
http://127.0.0.1:7477`. Without it the phone shows a message naming both
`daisugi voice serve` and `--voice-url`.

## Grant a pane direct send

`daisugi voice ptt` and the phone's Record button both always preview.
Delivered text goes to the pane's prompt box, and you read it and send it
yourself, whether or not the pane is armed.

```bash
daisugi voice arm w1:p1 --for 30m
```

Arming grants a time-boxed window read by a caller that posts `"mode":
"send"` to `/deliver`. Neither `voice ptt` nor the phone's Record button
uses this yet. The grant expires on its own. Revoke it early.

```bash
daisugi voice disarm w1:p1
```

Add `--json` to either command for a machine-readable line: `arm --json`
prints the pane and the expiry as an epoch value; `disarm --json` prints the
pane and whether a grant actually existed to remove.

## Reach the server from another box

Pass `--listen` to bind an address other than `127.0.0.1`, for example on a
tailnet.

```bash
daisugi voice serve --listen 0.0.0.0:7477
```

Every request needs a bearer token now, loopback included. The server reads
the token file `coppice web token` writes, under
`<data-dir>/coppice/web/token`. Pass `--token-file` to name a different
file. Every caller, on this box or another, sends that token in an
`Authorization: Bearer` header.

`--data-dir` decides both this token file and the armed directory `arm` and
`disarm` write into. Serve, arm, and disarm must all name the same data
directory, or a grant made one way is never seen the other way.

Direct send reaches a pane through the coppice socket under
`<data-dir>/coppice`. `voice serve` has no `--socket` flag of its own;
`--data-dir` is the only way to move it.

## Clean up the transcript

Off by default. Turn it on in `~/.opendaisugi/config.yaml`.

```yaml
voice_cleanup: true
voice_cleanup_model: ollama/llama3.2:3b
```

This runs one fixed-prompt call on the model you name. It never uses a paid
model unless you name one. It never changes what you said. It only fixes
punctuation and obvious transcription slips. A model that fails to answer
never loses your words. The raw transcript still reaches the pane.

## CPU and GPU

`voice_device` stays `cpu` by default. Setting it to `cuda` only takes effect
when a CUDA device is actually visible on this box. Otherwise it runs on CPU
with no error. Some GPUs have crashed other local model inference under CUDA.
A working CPU path beats a broken GPU one.

## A second engine: Parakeet

faster-whisper is the default engine. Parakeet is a second engine you can opt
into. It needs the `opendaisugi[voice-parakeet]` extra, which installs
`sherpa-onnx`, and a downloaded model directory. The model archive is about
460 MB. Set `voice_engine` to `parakeet` and `voice_model` to the extracted
model directory to switch. `daisugi voice serve` reads only the engine you
configured, so a Parakeet-only box never needs faster-whisper installed.

## What leaves the box

Nothing, by default. The clip is decoded and transcribed in process, the
grant files under `<data-dir>/voice/armed` are local, and the token file is
local. With cleanup on, the transcript is also journaled to
`<data-dir>/gateway/turns.jsonl`, exactly as a typed prompt is. The one
outbound call in the whole path is the optional cleanup pass, off by
default, which goes wherever `voice_cleanup_model` and
`voice_cleanup_base_url` point. Naming a hosted model there is the one way
your transcript leaves your own machines.

## If something goes wrong

| You see | Next step |
|---|---|
| `pane not armed for direct send. Run: daisugi voice arm {pane} --for 30m` | Run the `voice arm` command it names, or keep reading the text from the prompt box yourself. |
| `no pane backend is available. ... Start one: coppice server start, or install tmux.` | Start coppice, or install tmux, then try again. |
| `refusing to listen on {address} without a token. {token_file} is missing or unreadable. Run coppice web token first, or pass --token-file.` | Run `coppice web token`, or pass `--token-file`. |
| `This request needs a bearer token. Run coppice web token on the box that runs this server.` | Run `coppice web token` on the server box, then send that token. |
| `Too many failed tokens from this address. Wait 60 seconds and try again.` | Wait a minute, then check the token you are sending. |
| `This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A.` | Re-record or re-encode the clip into one of those formats. |
| `The clip is longer than 60 seconds. Send a shorter clip.` | Record a clip under 60 seconds. |
| `Could not reach the voice server at {url}. Run daisugi voice serve first.` | Start the server the message names. |
| `Voice is not set up. Run daisugi voice serve, then start coppice web serve again with --voice-url.` | Run `daisugi voice serve`, then start `coppice web serve` again with `--voice-url` pointed at it. |
| `The clip is larger than 8388608 bytes. Record a shorter clip.` | Record a shorter clip. |
| `The clip could not be read. Record it again.` | Try recording it again. |
| `No token file at {path}. Run coppice web token on the box, or pass --token-file.` | Run `coppice web token`, or pass `--token-file`. |
| `No audio was captured. Record for longer before tapping space again.` | Tap space, wait longer, then tap it again. |
| `No speech was heard. Record for longer before tapping space again.` | Speak closer to the microphone, then tap space, wait longer, and tap it again. |
| `No speech was heard. Hold Record longer and speak closer to the microphone.` | On the phone: speak closer to the microphone, or hold Record longer before tapping it again. |
| `The cleanup model returned no text. Check the model, then try again.` | Check the cleanup model is right. The raw transcript already reached the pane. |
| `The cleanup model failed to respond. Check that it is running, then try again.` | Check the cleanup model is running. The raw transcript already reached the pane. |
| `The microphone was not allowed. Allow it for this page, then tap Record again.` | Allow microphone access for this page in the browser, then tap Record again. |
| `This browser cannot record audio. Try a newer browser or device.` | Try a newer browser, or a different device. |
