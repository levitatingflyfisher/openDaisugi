# Record anywhere, land it in a pane

`daisugi voice` turns a recording into text in the pane you choose. You always
see the text before the pane's agent does. Direct send stays off for a pane
until you turn it on.

## Turn it on

Create a token, then start the server. Every request needs the token,
including one from this same box, so `daisugi voice serve` will not start
without it.

```bash
coppice web token
daisugi voice serve
```

This binds `127.0.0.1:7477`. It answers `GET /health`, `POST /transcribe`, and
`POST /deliver`. coppice starts it for you, so on a floor you often need
neither command.

With no voice settings in `config.yaml` (or `voice_engine: faster-whisper`
with `voice_model: tiny.en`, the defaults), the engine follows the box's
hardware, and the server says in one line which it chose and why:

- Parakeet v2 on a desktop with at least 8 GB of RAM and 4 cores;
- else Moonshine small with at least 2 GB;
- else faster-whisper `tiny.en` where the Python build has its extra
  (`uv add 'opendaisugi[voice]'`), and Moonshine small otherwise.

The first of these whose program is installed wins, so a box without
`parakeet-cli` gets Moonshine, and the line says that `parakeet-cli` is
not on PATH. The Go and Rust daisugi never load faster-whisper. Each
engine says on first use that it is fetching its model, and how big it
is: Parakeet v2 fetches 1237 MB once and keeps a 397 MB file, Moonshine
small is 142 MB, faster-whisper's `tiny.en` 78 MB. faster-whisper keeps
its model in the Hugging Face cache (`~/.cache/huggingface`); Parakeet's
and Moonshine's stay in `~/.cache/opendaisugi/models` (or under
`$XDG_CACHE_HOME`). If the fetch fails, the message says so; run `daisugi
voice serve` again to retry.

Parakeet and Moonshine run as `parakeet-cli` and `moonshine-cli`, small
programs `scripts/install.sh` builds (`clients/go/scripts/native.sh
--parakeet` and `--moonshine`) and links onto PATH. Each runs as one
child of the voice server that loads its model once, before the server
listens, and keeps it loaded. If the child stops, the server starts it
again; a clip that comes while it loads gets `503 engine_loading`. Try
again a moment later.

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

## Choose an engine

| `voice_engine` | `voice_model` | Needs |
|---|---|---|
| `parakeet` | `v2` (the default), fetched once at a pinned digest and made Q4_K on this box; or the path of a Parakeet-TDT GGUF file | `parakeet-cli` on PATH (and `parakeet-quantize` beside it, for `v2`) |
| `moonshine` | `tiny`, `small` (the default) or `medium`, fetched once at a pinned digest; or a Moonshine streaming model directory | `moonshine-cli` on PATH |
| `faster-whisper` | a model name such as `tiny.en` or `base.en` | the `[voice]` extra; the Python build only |
| `whisper.cpp` | the path of a ggml model file | `whisper-cli` on PATH; it runs once per clip |

On this box (a 4-core 2017 laptop CPU), on real speech, Parakeet v2
answers a 5 s clip in about 1.1 s with 2.5 to 3.5 times fewer errors than
`tiny.en`; Moonshine small takes about 1.5 s; `tiny.en` about 0.45 s. See
docs/research/stt-2026-10.md, "Measured again with the resident engines".

Parakeet v2 is NVIDIA's model, under CC-BY-4.0; the attribution is in
NOTICE.

Every model is fetched at a pinned Hugging Face commit (or, for Moonshine,
a dated directory) and each file is checked against its sha256.

### Parakeet v2 on aarch64

The Q4_K file that `parakeet-quantize` makes is pinned for x86_64 only. On
any other machine, `voice_model: v2` stops with one line and fetches
nothing; the path of a Parakeet GGUF file still works. To pin the aarch64
result, on an aarch64 box (or under `qemu-aarch64` user-mode emulation):

1. Build the engine: `clients/go/scripts/native.sh --parakeet` (every file
   but the ggml CPU code is built for the baseline CPU with
   `-ffp-contract=off`).
2. Fetch the F16 file at the pinned commit, and check it:

   ```bash
   curl -LO https://huggingface.co/cstr/parakeet-tdt-0.6b-v2-GGUF/resolve/8878172f3c45ba231fac8cfd4aff2542b13bc875/parakeet-tdt-0.6b-v2.gguf
   sha256sum parakeet-tdt-0.6b-v2.gguf   # c82b001dcb0adecd36f7401e4b77c7257eb352462368b89cf3ee13184206d7f7
   ```

3. Make the Q4_K file and hash it:

   ```bash
   "$(clients/go/scripts/native.sh --print-parakeet)/bin/parakeet-quantize" \
     parakeet-tdt-0.6b-v2.gguf q4k.gguf q4_k
   sha256sum q4k.gguf
   ```

4. If the hash is the x86_64 one
   (`764c4e6738b0b38c53bbfea040f9e07425d6d742df58b18906053085aea46b1c`),
   add `aarch64` to `PARAKEET_QUANT_ARCHES` in `voice/pins.py`, Go's
   `ParakeetQuantArches` and Rust's `PARAKEET_QUANT_ARCHES`. If it is not,
   the quantized pin becomes one per machine: record the new hash beside
   the old one in all three. Run it twice to be sure the result is stable.

## What leaves the box

No audio and no text, by default. The clip is decoded and transcribed on
this box (in process, or by `parakeet-cli`, `moonshine-cli` or `whisper-cli`), the grant
files under `<data-dir>/voice/armed` are local, and the token file is
local. With cleanup on, the transcript is also journaled to
`<data-dir>/gateway/turns.jsonl`, exactly as a typed prompt is. The first
use of a model fetches it: faster-whisper's and Parakeet's from Hugging
Face, Moonshine's from download.moonshine.ai. Those requests name the model file and carry
nothing of yours. The one outbound call that carries your words is the
optional cleanup pass, off by default, which goes wherever
`voice_cleanup_model` and `voice_cleanup_base_url` point. Naming a hosted
model there is the one way your transcript leaves your own machines.

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
