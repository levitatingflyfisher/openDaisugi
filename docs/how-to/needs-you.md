# Answer from the floor

You talk to the steward. You seldom open an agent. The floor shows you what
waits on you, lets you answer from where you are, and keeps itself tidy.

## The Needs you cards

An agent that waits on you has a card at the top of the floor. The card shows
on a phone and on a desktop. It names the agent, says the ask in one line, and
has the way to answer. Every agent also stays one compact row below.

| The agent | The card shows | You |
|---|---|---|
| asks for a tool permission | the command, the warden's line | Deny, Look, Allow |
| asks to trust a folder | the folder | Trust this folder, Not now |
| is idle and ended on a question | the question | type an answer, Send |
| is blocked on its own screen | a line that says so | Open |
| the steward proposes an envelope that asks for more than the project's default | each way it is wider, and the verbs that stay denied | Approve, Decline |

A known project needs no envelope card. The steward grants it the default
permission by itself: the project dir, shell commands, no budget, and no
push, release, deploy or money. A card appears only when a proposal asks for
more, or when the project is outside the trusted roots. See
[default-permission.md](default-permission.md).

A card goes when you answer it. If an answer fails, the card stays and the
status line says why. After each action the status line says what changed in
one line, for example `Sent to build agent.`

The question card reads the last line of the agent's last message. It shows
when that line ends in a question mark and you have not answered since.

## Ended agents tidy themselves

An agent that exits 0 leaves Recent by itself. An agent that failed, was
killed, or ended with no exit code the server knows stays in Recent, with the
reason, until you press Forget. `Show N finished` brings the clean ends back
for a look. Their history is in Pando. Stopping an agent with the x forgets it
at once.

## The floor never answers a prompt by itself

The floor shows what waits on you. It never presses a key on a prompt for
you: not a trust screen, not a tool permission, not a question. An agent that
the steward made and that stops at a folder trust screen shows the same Trust
card as any other agent. You read the folder on the card and tap Trust this
folder or Not now.

The server marks a pane as made by an agent (`made_by`) when a pane's
connection created it. The mark stays on a resumed pane. It changes nothing
about who answers.
