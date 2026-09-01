# Spec 06 — The phone: a PWA served by coppice-server, push through ntfy

**Master:** §5.6, §3.3, §3.6 · **Size:** L · **Depends on:** 02, 03
**Prerequisite (checked by plan task 0):** a tailnet, or a LAN, between the phone and the box.

## Purpose

From the phone, in the kitchen: see every pane and its state, open one, read it, prompt or steer
it, allow or deny an ask, and be told when something blocks. No app store, no relay, nothing of
Google's in the path by default.

## The cruxes

- **A PWA, not an app.** One codebase, installable, served by the same binary that owns the
  panes. The secure context is the only hard part, and there are two honest ways to get it.
- **ntfy for push.** Web Push on Android transits Google. ntfy is Apache-2.0, one binary, and its
  app keeps a websocket to our server. Web Push stays available behind a flag for people who
  accept the trade.
- **A bearer token, shown once as a QR.** The socket is uid-bound; HTTPS is not. The PWA
  authenticates with a token the server mints and the terminal shows as a QR. Tailscale grants are
  the outer wall; the token is the inner one. Both, not either.

## Files

```
harness/coppice/internal/web/server.go          # HTTPS listener; /ws (JSONL over websocket); /api/token check; static
harness/coppice/internal/web/tls.go             # cert sources: tailscale | localca | files
harness/coppice/internal/web/push.go            # ntfy publisher on `blocked`; optional Web Push (VAPID)
harness/coppice/internal/web/static/            # index.html, app.js, grid.js (canvas), sw.js, manifest.webmanifest, icons
harness/coppice/cmd/coppice/web.go              # `coppice web serve|token|cert`
docs/how-to/phone.md                            # the setup, both cert paths, the exposure note
tests: harness/coppice/internal/web/*_test.go ; a Playwright pass via the visual-loop skill (scripts/phone_smoke.py)
```

## Serving

`coppice web serve [--listen :8443] [--tls tailscale|localca|files] [--ntfy URL --ntfy-topic T
--ntfy-token-env NAME]`. Runs inside the server process (a `web.enabled` setting persists it) so
there is one process to keep alive. `/ws` speaks exactly the §3.3 JSONL, one websocket = one
socket client, with `pane.attach` frames flowing as JSON. Static assets are `embed`ded. The
service worker caches the shell only, never pane content.

Auth: `Authorization: Bearer <token>` on `/ws` upgrade and every `/api/*`. `coppice web token`
mints (or rotates) a 32-byte token stored 0600 and prints it as text and as a terminal QR
(`qrterminal`). The PWA stores it in `localStorage` after the first paste or scan. A bad token is
`401` and a log line; three bad tokens from one address in a minute is a 60 s ban.

## TLS

- `tailscale`: reads the cert and key produced by `tailscale cert <name>.<tailnet>.ts.net` from
  the paths the docs name; the how-to states the admin-console steps (MagicDNS on, HTTPS on) and
  the fact that the machine name lands on a public certificate-transparency ledger. Renewal is
  the operator's; `coppice web serve` warns 14 days before expiry.
- `localca`: `coppice web cert init` creates a CA and a leaf for the LAN name/IP under
  `~/.opendaisugi/coppice/ca/`, prints the CA cert as a QR for the phone to install once, and
  documents the Android "install CA certificate" steps. This is the exposure-safe path.
- `files`: explicit `--cert/--key`.

## The client

Plain JS, no framework, no bundler (assets are embedded; a build step is one more thing to
break). Screens: **Roster** (pane cards with state chip, harness, label, age, ask summary when
blocked; tap → Pane), **Pane** (canvas grid from frames, pinch-zoom, a text box with `Prompt` and
`Steer` buttons, `Allow`/`Deny` when blocked, `Keys` drawer for enter/esc/ctrl-c/tab), **New**
(cwd picker from recent cwds, harness picker, label), **Settings** (server URL, token, ntfy topic
test button). The record button lands in spec-07.

State chips use colour and a word (never colour alone). The roster orders `blocked` first.

## Push

On every merged transition to `blocked`, the server `POST`s to `<ntfy>/<topic>` with title
`<label> needs you`, body the ask summary, and a click URL to the pane. Debounced 5 s per pane.
Auth to ntfy by the token in the named env var. Web Push (`--web-push`) is off by default and
documented as "transits your browser vendor's push service".

## Tests

- Go: token auth (401/ban), websocket JSONL parity with the unix socket (same request → same
  response bytes), ntfy publish called once per `blocked` transition with the expected body, TLS
  source selection, `localca` init produces a valid chain.
- Playwright (visual-loop skill): load over `localca` in a headless Chromium with the CA trusted,
  paste token, see the roster, open a pane, send a prompt to a `sh` pane, see the echo in the
  canvas; a screenshot per screen at 360×780 checked into `testdata/phone/` for eyeballing.

## Out of scope

Native app; iOS specifics beyond "it is a PWA"; multiple users; Herdr Mobile.
