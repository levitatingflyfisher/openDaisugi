# Bring your old Claude Code and Codex chats into the steward

The steward can know what you said in earlier chats. It does this only if you
choose. Nothing is read before you say yes. This needs the steward in chat mode
(`[steward] mode = "chat"`).

Where the text goes: imported messages are summarized by the steward's model
like any chat message. Until a message is summarized, it is in the raw tail that
each turn sends to the turn model. If that model is a remote one, your old text
goes there. Choose with that in mind.

## Say yes

When the chat opens and old chats exist, the steward says so once. The floor and
the phone show a bar: `Import 12 past chats (about 3400 messages)?` Press
Import. Press No thanks and it stays quiet; the command below still works.

Or from a terminal. The importer is part of daisugi, so it works with no
coppice running:

```sh
daisugi pando import --dry-run       # count them. No model call, no write.
daisugi pando import                 # queue and log every chat with new messages
daisugi pando tend                   # then build the summary nodes
```

`tend` with no `--profile` runs Claude Code, one paid call for each node. A
long history needs thousands of calls. Over 25 nodes, `tend` prints the count
and stops until you choose: `--profile NAME` (an agent profile, such as a local
model) or `--yes` (accept the default and pay). `--max N` builds at most N
nodes in one run, oldest first, so you can do a part and look at the cost. The
same applies to `import --tend`. If the model gives no text for some nodes,
`tend` counts them (`fell back to clipped: M`) and says why; see
[pando.md](pando.md) for `max_tokens` and `tend --redo-clipped`.

Options: `--from claude|codex|web|all`, `--dir PATH`, `--since 2026-01-01`, `--tools` (also tool
calls and their results; off by default), `--store NAME` (default `work`). A
model's thinking is never taken. `coppice steward import` still works and runs
the same code on the `work` store.

## Chats in a folder: `--dir`

The importer reads `~/.claude/projects`. To read chats from another place,
for example sessions copied from another machine, name the folder:

```sh
daisugi pando import --dir ~/old-sessions --dry-run
daisugi pando import --dir ~/old-sessions
```

`--dir PATH` holds Claude Code chats in either form: project folders
(`PATH/<project>/<session>.jsonl`, the layout of `~/.claude/projects`) or
session `.jsonl` files straight in `PATH`. It replaces `~/.claude/projects`
for this run, and Codex chats are not read. Use it with `--from claude` or
with no `--from`; any other source is refused. Everything else works as with
the default folder: `--since`, `--tools`, `--map`, `--tend`, `--dry-run`.
Nothing is written to `~/.claude`.

A message is logged once, by its session id and its message id. A copied or
forked Codex rollout keeps its lines once.

## Chats from another machine: `--map`

A transcript names the project dir it ran in. On another machine that path is
different. `--map OLD=NEW` rewrites the front of that path, by path element, so
the same project gets the same name:

```sh
daisugi pando import --from claude --map /home/old=/home/new
```

It touches the project path only. It never changes message text.

## Chats from the claude.ai site

Export your data from the site and keep the zip on this machine. Then:

```sh
daisugi pando import --from web --web export.zip --propose chats.tsv
```

This writes `chats.tsv` and changes no store. Each line is
`key<TAB>store<TAB>reason<TAB>title`. Edit the second field to `work`, `home`
or `skip`. Then:

```sh
daisugi pando import --from web --web export.zip --accept chats.tsv
```

Only now do messages enter a store, each chat in the store its line names. A
web chat is keyed by its conversation `uuid` and each message by its own
`uuid`, so a run twice logs nothing twice. A line that says `all` is refused.

## Watch it

The bar says `Importing past chats: 12 of 3400 messages`. In the terminal
interface, the bar at the top says `importing 12/340`. In a shell:

```sh
coppice steward import --status          # running, paused, done
coppice steward import --pause           # stops at the next message
coppice steward import --resume
```

It goes one message at a time. No new model call for an old message starts while
a turn of the steward runs or waits. A call that already runs finishes. A live
message is summarized before any old message queued earlier, unless it needs
that old message as input. Old text still in the raw tail goes to the turn model
with every turn, as said above. Restart coppice and it goes on where it stopped.

## Run it again later

Coppice's own model calls are not old chats: a session that lives in coppice's
own dirs, or that is a compactor call, is left out. Run `coppice steward import`
again after new chats. Only new messages come in:
each message has an id from its transcript, and a message in the log is never
logged twice. A Codex rollout that is copied or forked keeps its lines once: a
line is known by its session id and its own text. The numbers in the offer may
count such a copy twice; the log does not. Codex chats that an earlier
pre-release build imported are imported once more, because their message ids
changed.

Pressing Import scans again when the chats changed since the last scan or the
scan is over a minute old. If nothing is new, the bar says "Nothing new to
import."

## Where the old messages are

They are in the steward's log as kind `past`, dated when you said them. The
floor's chat does not list them. The steward finds them like any other message,
with its `zoom` and `date` tools. `coppice steward log --day 2026-03-01` lists
them.

Rulings: FM-R-71 to FM-R-92 in `clients/ADJUDICATIONS.md`.

## Chats from pi and OpenCode

`daisugi journal parse` reads these two formats for chats recorded before you
installed daisugi. Each run parses one session:

```sh
daisugi journal parse ~/.pi/agent/sessions/<project>/<session>.jsonl --format pi -o eps.yaml
daisugi journal parse ~/.local/share/opencode/storage/session/<project>/<id>.json --format opencode -o eps.yaml
daisugi journal ingest eps.yaml
```

pi writes one JSONL file per session (format version 3; see
`pi.dev/docs/latest/session-format`). OpenCode writes one JSON file per session,
message and part. The OpenCode command takes the session file and finds its
messages in the `storage/message` and `storage/part` trees beside it. The OpenCode
layout has changed between releases, and the current release may not write it.
That is not verified; a parse of a session without messages exits 2 with
`no OpenCode messages for session <id>`.
