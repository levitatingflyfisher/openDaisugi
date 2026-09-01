# The protocol corpus

Each `*.jsonl` file here is a conversation with a coppice server. The Go test
`internal/proto/conformance_test.go` replays every file against a real server
and checks each reply. A client in another language replays the same files
to prove it speaks the same protocol. The matching rules are implemented in
`internal/proto/conformance_match.go`. This page states the same rules in
prose. When the two disagree, the Go file is wrong and must be fixed to match
this page, or this page is wrong and must be fixed to match the Go file. Do
not let them drift.

## File shape

- One JSON value per line.
- Blank lines are ignored.
- A line that starts with `#` is a comment and is ignored, except a line that
  starts with `#skip` in the expected position. See below.
- The remaining lines alternate: a request line, then its expected reply line.
- A file with an odd number of lines is a broken file. The replay fails it.

## Replay

- Files are replayed in file name order.
- Each file is replayed on ONE connection, opened before the first request
  and closed after the last reply. Connection state carries across the cases
  in a file. A pane attached in case 2 is still attached in case 5. The
  framing a file chose stays for the whole file.
- The replay sends one request, then reads lines until it finds one with an
  `id` key. That line is the reply. A line with no `id` key is an event on a
  native connection or a notification on a JSON-RPC connection. The replay
  skips it and keeps reading.
- A reply with `"id": null` still has an `id` key. It is a reply, not an event.

## Matching an expected line

The replay decodes the reply and the expected line as JSON and compares them
with these rules:

- The string `"*"` in the expected line matches any present value of any
  type. It does not match an absent key.
- The string `"$id"` in the expected line matches only the `id` value the
  request line carried. A numeric id must come back numeric.
- Any other string `"$name"` in the expected line matches any present value
  and binds that value under `name`. A later binding of the same name replaces
  the earlier one.
- An expected object matches when every key it lists is present in the reply
  and its value matches. Extra keys in the reply are allowed.
- An expected array matches a reply array of the same length whose elements
  match one to one.
- Every other value matches only an equal JSON value. `false` does not match
  `true`. `null` does not match `0`. `"7"` does not match `7`.

## Substitution in a request line

Before the replay sends a request, it replaces every string `"$name"` in the
request, at any depth, with the value bound under `name` by an earlier
expected line of the same file. A name with no binding fails the replay. A
request may not use `"$id"`. The string `"*"` is sent as it is.

Example. Case 1 binds the pane id, case 2 uses it:

```
{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["sh"],"kind":"pty"}
{"id":"$id","ok":true,"result":{"pane":"$p"}}
{"id":"2","cmd":"pane.read","pane":"$p"}
{"id":"$id","ok":true,"result":{"text":"*","source":"visible"}}
```

## Skips

An expected line that starts with `#skip` marks a case the server does not
implement yet. The text after `#skip` is the reason. The replay does not send
that request. It logs the case as a gap and moves on. A skip is never a pass.
A replay that reports zero failures and one skip has one known hole, and the
report must say so.

## Framing

A file is written in one framing. The native envelope and the JSON-RPC 2.0
form are both described in `../../PROTOCOL.md`. `pane-create.jsonl`,
`flow.jsonl`, `fork.jsonl`, `notes.jsonl`, `tasks.jsonl` and `view-only.jsonl` are native. `jsonrpc.jsonl` carries the
`pane-create.jsonl` cases in JSON-RPC form. `acp-adjacent.jsonl` is JSON-RPC. It sends the verbs that
sit beside four ACP methods.
