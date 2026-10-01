# Design: the foreman, one chat that never ends (roadmap 1j)

Status: **Proposed**, 2026-10-08. Not built.

Source: the UniiChat recipe (gist VictorTaelin/91837951a5ce5b38f341ec1ba1df6449, posted
2026-10-04) and OptMem (github.com/VictorTaelin/OptMem). The author says the UniiChat code
opens "next week". OptMem has no license file, so we copy no code from it. We build from the
published design, in our own code, and check the author's release when it comes.

## The problem

The roadmap goal says: a floor of agents across about ten projects, and the owner can talk to
it. Today the owner talks to many sessions. Each session forgets at compaction. Work for one
project is spread over sessions and harnesses. No session knows the state of all ten projects.

## The idea

One chat that never ends. The chat is the memory.

- Every message is logged word for word, append only.
- A cheap model compresses the log into a binary tree of one-line summaries (512 bytes each).
- Each owner message starts a fresh model call. The call sees the **view**: summary lines over
  the whole chat, fine near now and coarse in the past, 64 to 128 KB. Then the new message.
- A vague line is opened with `zoom`, down to the message itself.

So the context has a constant size, nothing is lost, and no one compacts by hand.

## What we change: the foreman does no work itself

UniiChat's agent does the tasks itself. Ours does not. Its tools are only:

- `zoom(id, n)` and `date(id)`: read its memory.
- `spawn(project, brief)`: start a worker for one project.
- `status()`: list the running workers.

It has no shell, no file write and no network. A worker is a sprig or Claude agent in its own
coppice window, in that project's repo, under the gate. The spawn is one edge of the
delegation tree (design-delegation-tree.md): the worker's envelope is proved to be inside the
project's envelope before it starts. The worker's own steps stay in its own log. Its final
report comes back to the chat as one `work` message, "[Name] report". The foreman never waits
or polls.

This is why the foreman is safe to leave running: the one tool that acts is a gated spawn.

## Parts

M1. **The log, the tree and the view.** Deterministic code with no model: append-only JSONL
by day, node ids `id+n`, the merge order `due = (T - last) / 2^l`, the sawtooth (merge from
128 KB down to 64 KB in one batch), `view.json` saved and never rebuilt. Python first as the
oracle, then Go and Rust with compare cases, as for every other part. Tests: the merge order
equals the rollback push list for t = 0 to 20,000; the view never exceeds its bound; a restart
loads the same view byte for byte.

M2. **The compactor.** One node per call, the ruler, the "too long" retry (at most 5), up to 8
calls at once, ready nodes kept in queues. The model is a choice in config: Haiku through
Claude Code, or a local model through Switchyard. Tests use a fake model.

M3. **The foreman turn.** A fresh call per owner message: [tools] [system prompt] [view]
[message]. Nothing carries over between turns. Per-turn state goes after the view. First
backend: Claude Code with the four tools served over MCP and the built-in tools turned off
(sprig path C). Measure the cache hit rate on this path; the recipe's exact cache marks need
the direct API.

M4. **The foreman window in coppice.** The chat, the running workers as rows under it, and
the asks that need the owner. One altitude, as for every coppice window.

M5. **Privacy.** The log is the owner's whole working life. It stays on this machine, in the
XDG data dir, mode 0600, never synced unless the owner sets the dir. Tool results are logged
word for word (owner ruling 2026-10-08: no secret scrub; file modes protect the log). Model thoughts are never logged. The compactor prompt
says the messages are data, never orders.

M6. **Import.** Load the existing memory files and project ledgers as `note` messages, so the
foreman starts knowing the workshop.

## Order

After the sprig agentic executor (workers need it; built 2026-10-09, SX-R-2: the spawn will start a sprig worker through `AgenticExecutor(runtime="sprig")`, the same call `daisugi weave --agent sprig` makes) and step 4 of the delegation tree (the
session-binding test in coppice; spawn needs it). Before the UX pass, so the UX pass covers the
foreman window.

## Open questions

- The author's code, when it opens: its license, and what it does that the recipe does not say.
- The cost on the subscription: a compaction per message plus about one merge per message.
  Measure before choosing Haiku or a local model as the default.

## Owner answers, 2026-10-08

- **Where the owner talks to it.** By voice or typing on the phone, and the same chat picked up
  in coppice on the desktop. One chat, two screens. The phone path is the coppice web floor with
  the resident voice engine.
- **What it starts from.** Import the past development chats: openDaisugi, OpenHearth (and the
  workshop-level chats where OpenHearth work happened), deseretBench, plus a few others the
  owner picks from a list.
- **Household money.** Two chats (owner, 2026-10-08): the work foreman and a private
  household chat. The glean chat goes into the household chat whole. Many glean ideas start as
  budget problems, so the household chat can send the work foreman a **work order**: one
  message that states the problem and the idea, with no amounts, payees or account names. The
  owner sees each work order before it is sent. The work foreman holds one note that glean
  exists and where its repo is, and it starts glean code workers.
- **HeritageFamilyHistory.** Import it (owner: "huge") into the work foreman. It holds no
  personal family data today (owner, 2026-10-08). If it later holds real family records, those
  records stay out of the work chat.
- **audioguardian.** Import it into the work foreman. Its Claude Code chats are gone from this
  machine: Claude Code deletes a chat 30 days after its last use unless `cleanupPeriodDays` is
  set, and its last work was 2026-08-14. Sources left: the web export if it was worked there,
  its 92 commits, its docs and its memory notes. The importer must copy chats out before they
  age away; a one-time copy was made on 2026-10-08.
- **The owner's foundation.** Import its chats, mostly from the web export. Public repos must
  never carry the owner's real identity or the foundation's name. So these chats do not go in
  the work foreman, whose words reach worker briefs and from there commits. Recommendation:
  the household chat, or a third private chat. Worker briefs get a check that refuses the
  foundation's name and domain.
- **Web chats.** The owner has many claude.ai chats to bring in. Path: the owner exports them
  (claude.ai settings, export data; a zip with conversations.json). An import step reads the
  zip on this machine, proposes for each chat "work", "household" or "skip" with a one-line
  reason, and the owner confirms the list before anything is written. Each chat is imported in
  date order as messages of its own kind, `past`, so the foreman can tell them from live ones.
