# Bring your old Claude Code and Codex chats into the foreman

The foreman can know what you said in earlier chats. It does this only if you
choose. Nothing is read before you say yes. This needs the foreman in chat mode
(`[foreman] mode = "chat"`).

Where the text goes: imported messages are summarized by the foreman's model
like any chat message. Until a message is summarized, it is in the raw tail that
each turn sends to the turn model. If that model is a remote one, your old text
goes there. Choose with that in mind.

## Say yes

When the chat opens and old chats exist, the foreman says so once. The floor and
the phone show a bar: `Import 12 past chats (about 3400 messages)?` Press
Import. Press No thanks and it stays quiet; the command below still works.

Or from a terminal:

```sh
coppice foreman import --dry-run     # count them. No model call, no write.
coppice foreman import               # queue every chat with new messages
```

Options: `--from claude|codex|all`, `--since 2026-01-01`, `--tools` (also tool
calls and their results; off by default). A model's thinking is never taken.

## Watch it

The bar says `Importing past chats: 12 of 3400 messages`. In the terminal
interface, the bar at the top says `importing 12/340`. In a shell:

```sh
coppice foreman import --status          # running, paused, done
coppice foreman import --pause           # stops at the next message
coppice foreman import --resume
```

It goes one message at a time. No new model call for an old message starts while
a turn of the foreman runs or waits. A call that already runs finishes. A live
message is summarized before any old message queued earlier, unless it needs
that old message as input. Old text still in the raw tail goes to the turn model
with every turn, as said above. Restart coppice and it goes on where it stopped.

## Run it again later

Coppice's own model calls are not old chats: a session that lives in coppice's
own dirs, or that is a compactor call, is left out. Run `coppice foreman import`
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

They are in the foreman's log as kind `past`, dated when you said them. The
floor's chat does not list them. The foreman finds them like any other message,
with its `zoom` and `date` tools. `coppice foreman log --day 2026-03-01` lists
them.

Rulings: FM-R-71 to FM-R-92 in `clients/ADJUDICATIONS.md`.
