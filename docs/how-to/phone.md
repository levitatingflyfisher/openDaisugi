# Use the phone

See every pane, open one, prompt it, and answer the gate, from a phone in the
kitchen. No app store, no relay, and nothing of Google's in the default path.

## Before you start

- coppice-server runs on the box. Check with `coppice server status`.
- The phone reaches the box. A tailnet is the usual way. A LAN works too.
- You have picked a certificate path. Read the next section before you choose.
- `coppice web serve` runs in the foreground and holds its terminal until you
  stop it with Ctrl-C. Run the rest of a path's commands from a second
  terminal once it is up, or skip ahead to "Keep it running" and let the
  coppice server hold it for you instead.

## Which certificate path

A phone will not install a web app, and will not run a service worker, over
plain HTTP. So the box needs a certificate the phone trusts. There are two
honest ways to get one.

| | `tailscale` | `localca` |
|---|---|---|
| Who issues it | Let's Encrypt, through Tailscale | your own box |
| What leaves the box | your machine name, onto a public certificate ledger | nothing |
| Phone setup | none | install one certificate, once |
| Renewal | yours, every 90 days | ten years |
| Works off the tailnet | no | yes, on any LAN |

Pick `tailscale` if publishing `<machine>.<tailnet>.ts.net` on a public ledger
is fine with you. Pick `localca` if it is not.

Two more `--tls` values exist for other cases. `files` serves a certificate
and key you already hold; give `--cert` and `--key` yourself. `off` serves
plain HTTP and only binds a loopback address. A browser treats
`http://127.0.0.1` as a secure context; no other plain HTTP address gets
that treatment. It is for testing on the box itself, not for a phone in
another room.

## Path A: tailscale

1. Open the Tailscale admin console. Go to the DNS page.
2. Turn on MagicDNS.
3. Under HTTPS Certificates, choose Enable HTTPS.
4. Acknowledge the notice. Your machine names go into a public ledger. Do not
   turn this on if a machine name is sensitive.
5. On the box, ask for the certificate:

```bash
coppice web cert tailscale box.tail1234.ts.net
```

That runs `tailscale cert` with explicit `--cert-file` and `--key-file` paths
and writes the pair under `~/.opendaisugi/coppice/web/tls/`.

6. Start the server:

```bash
coppice web serve --tls tailscale --external-url https://box.tail1234.ts.net:8443
```

No `--cert` or `--key` needed here. `--tls tailscale` reads the same pair
`coppice web cert tailscale` just wrote, from the same data directory. Pass
`--cert` and `--key` yourself only when the pair lives somewhere else.

Let's Encrypt certificates last 90 days. Renewal is yours. The server warns
in its log for the last 14 days.

## Path B: the local CA

1. Make the CA and the server certificate. Name every address the phone will
   use. If you give `--name`, the QR in the next step always carries the
   first one you gave, never an `--ip`, so give a name the phone can
   actually resolve. This example gives only `--ip`, which the QR then
   carries instead:

```bash
coppice web cert init --ip 192.168.1.20
```

   This prints a QR for the CA download, but nothing serves that download
   yet. Leave the output on screen.

2. Start the server. It runs in the foreground and holds this terminal:

```bash
coppice web serve --tls localca --external-url https://192.168.1.20:8443
```

   Once its log says it is serving the CA certificate, the QR from step 1
   is live.

3. Scan that QR, still visible above the server's own log lines. From a
   second terminal, `coppice web cert init --ip 192.168.1.20` prints the
   same QR again without disturbing the running server, if you need it
   back. The phone opens a download of `ca.crt` from the box on port 8080.
4. On the phone, open Settings, then Security and privacy, then Encryption
   and credentials, then Install a certificate, then CA certificate. Choose
   Install anyway, then pick the file you downloaded.
5. Android shows a standing notice that the network may be monitored. That is
   what a user-installed certificate authority always does.

The plain HTTP port that hands out the CA, `:8080` by default, listens on
every interface for as long as the server runs. Narrow it with `--ca-listen
127.0.0.1:8080`, or close it once the phone has the certificate with
`--ca-listen off`.

`--qr pem` draws the CA certificate itself as a QR, instead of a URL to
fetch it from. The URL form above is the only one this project has
exercised.

Re-run `coppice web cert init` whenever the box changes address. It keeps the
same CA and issues a new server certificate, so you never install a
certificate on the phone twice. The CA itself lasts ten years; the server
certificate it issues lasts 398 days and gets replaced by the same command,
with nothing to reinstall on the phone.

## Sign in

The server from Path A or B is still running in its own terminal. From a
second terminal:

```bash
coppice web token
```

That prints the token, a sign-in URL, and a QR. Scan the QR on the phone. The
app stores the token and clears it from the address bar. To retire a token,
run `coppice web token --rotate` and scan the new QR.

Three bad tokens from one address earn that address a minute of silence.

**Treat the token like an SSH key.** The phone speaks the same protocol your
terminal speaks, with nothing filtered, so anyone holding the token can start
a pane running any command in any directory. That is what makes the phone
useful and it is also what it costs. Rotate the token if you lose the phone.

Until `coppice web serve --persist` has saved a listen address and an
external URL, `coppice web token` guesses the sign-in URL from the box's own
hostname. Pass `--url` and `--listen` yourself if that guess is wrong.

Install the app from the browser menu, with Add to home screen.

The app always talks to the address it was served from. Settings shows that
address; there is no box to change it. To point a phone at a different box,
open that box's own URL.

Running two coppice servers, on one box or on two, means two phones.
`--data-dir` and `--socket` are the global flags that give a second instance
its own token, its own certificate directory, and its own gate root, all
under a data directory of its own. The two phones never share a credential.

## Push

Push goes through your own ntfy. Nothing else.

1. Run ntfy on the box or on the tailnet. One binary, Apache-2.0.
2. Install the ntfy Android app. Subscribe it to your topic.
3. Put the ntfy token in an environment variable and name it:

```bash
export COPPICE_NTFY_TOKEN=tk_yourtoken
coppice web serve --tls localca \
  --ntfy https://ntfy.box.local --ntfy-topic coppice \
  --ntfy-token-env COPPICE_NTFY_TOKEN \
  --external-url https://192.168.1.20:8443
```

The server publishes when a pane becomes blocked, with the pane label in the
title, the ask in the body, and a link to that pane. A pane stays quiet for
five seconds after it speaks.

The card has buttons when `--external-url` is set. Deny is first. It denies
the ask from the lock screen. Look opens the pane. An undoable ask also has
Allow, which opens the pane screen too, where one tap allows. A permanent ask
has no Allow on the card; open the pane and type its name. The Deny button
carries a token that can deny that one ask and nothing else, never your web
token, because ntfy keeps a copy of each button. The token dies when the ask
is answered or its deadline passes.

Test it from the phone. Open Settings, then Test push.

Web push is not built. It would route your notifications through your browser
vendor's push service, which is what ntfy exists to avoid here. See
`docs/feature-status.md`.

## Keep it running

```bash
coppice web serve --tls localca --persist \
  --external-url https://192.168.1.20:8443 \
  --ntfy https://ntfy.box.local --ntfy-topic coppice \
  --ntfy-token-env COPPICE_NTFY_TOKEN
```

`--persist` writes `~/.opendaisugi/coppice/web/web.json`, then serves in the
foreground exactly like a run without it. Stop this with Ctrl-C, then run
`coppice server start`. From then on the coppice server starts the phone
server with itself, so there is one process to keep alive instead of two.
`--forget` undoes it.

## What leaves the box

- With `tailscale`: your machine name, on a public certificate ledger.
- With `localca`: nothing on its own. While the server runs, though, a
  plain HTTP port, `:8080` by default, hands the CA certificate to anyone
  who asks, on every interface. Narrow it with `--ca-listen 127.0.0.1:8080`
  or close it with `--ca-listen off` once the phone has the certificate.
- With push: the pane label goes to the ntfy server you named, plus the ask
  summary, or the event's `detail` line when there is no ask. That detail can
  name a tool and a clause, for example `verdict=deny clause=shell.deny[2]`.
  A push with neither an ask nor a detail says so in plain words instead of
  going out blank. Every push also carries a link back to the pane, built
  from `--external-url` plus the pane's own id. Run your own ntfy and that
  is your box too.
- Nothing else. No telemetry, no relay, no accounts. No grid and no
  transcript ever leaves through push.

## When it does not work

| What you see | What to do |
|---|---|
| The browser says the certificate is not trusted | Install the CA. Run `coppice web cert show` to see what the certificate covers, then re-run `cert init` with the address you are actually using. |
| That token is not accepted. Run coppice web token and scan the QR again. | The token on the phone is stale. Do exactly that. The app stops retrying on purpose, so it does not get your own address banned. |
| No token. Open Settings and paste one. | Nothing is stored yet. Scan the QR from `coppice web token`. |
| Too many bad tokens. Waiting one minute. | Three wrong tokens came from your address. Wait, then scan the QR. |
| Reconnecting. | The box is unreachable. Check the tailnet or the LAN. The app backs off to thirty seconds and keeps trying. |
| coppice-server did not answer. Run coppice server status. | This is the roster's cold read, before the socket is open. The coppice server is not running, or `--socket` does not match. Run `coppice server status`, then `coppice server start` if it says nothing is running. |
| Add to home screen is missing | The page is not a secure context. Fix the certificate first. |
| No panes yet. Tap New to start one. | There really are no panes. A refusal is never shown this way; it shows on the status line as `server refused: ...`. Tap New to start one, or run `coppice pane list` on the box to confirm the server really has none. |
| The CA download will not open, or times out | Either `coppice web serve` is not running yet, since the CA hand-off port is only live while it runs, or the QR points at an address the phone cannot reach. Start the server first, then re-run `coppice web cert init` with the address the phone actually uses. |
| That ask is gone. The gate timed out, or another client answered it. | The answer is landing in the wrong directory, or the ask expired first. Check the `gate_root` the server logged at startup against your gate directory, and set `--gate-root` if they differ. It defaults to the data directory's own parent, plus `gate`: `~/.opendaisugi/gate` under the default data directory. |
| Push never arrives | Open Settings and press Test push. `Push is off. Restart the server with --ntfy URL and --ntfy-topic NAME.` means push was never configured. `ntfy did not accept the message. Check the URL and the token.` means ntfy itself refused it. `Sent. Watch the ntfy app.` means the box did its part; check the ntfy app's own subscription. |
