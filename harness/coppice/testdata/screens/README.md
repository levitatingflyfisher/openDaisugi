# Screen fixture format

These fixtures are shared. `internal/detect/manifests_test.go` in this repo reads them; plan 03's
Python evaluator reads the same files and must produce the same `(state, rule)` verdict for every
one of them. This file is the format both readers implement - verify before you rely, and if you
change the format, change it here and in both readers together.

## Layout

```
testdata/screens/<agent>/<state>-<N>.txt
testdata/screens/unfixtured.txt
```

- The **directory name** is the agent id: the manifest's own `id` field (e.g. `claude`, `codex`,
  `pi`, `opencode`), not an alias. `Set.For` resolves aliases, but a fixture directory names the
  canonical id.
- The **filename**'s part before the first `-` is the expected state: `idle`, `working`,
  `blocked`, or `unknown`. The `-N` suffix is just a serial number for more than one fixture at
  the same state (`idle-1.txt`, `idle-2.txt`, …); nothing reads the number itself.
- `unfixtured.txt` (sibling to the agent directories, not inside one) lists, one id per line, the
  vendored manifests with no fixture yet. `#`-prefixed lines and blank lines in it are comments.
  An id that isn't a real manifest id, or that already has a fixture directory, is a load-bearing
  error in `TestEveryManifestIsFixturedOrDeclaredUnfixtured` - the list is a to-do, not a
  write-once record.

## File contents: optional headers, then the screen

A fixture file is plain text. It may start with a contiguous run of header lines, one per line,
each beginning with one of exactly three recognized prefixes:

```
#rule: <rule id>
#osc_title: <text>
#osc_progress: <text>
```

**Headers are only headers at the top of the file.** The reader scans lines from the start of the
file, and a line matching one of the three prefixes above is consumed as a header. The moment a
line does *not* match one of those prefixes, the header run ends - that line, and every line after
it to the end of the file, is the screen text verbatim, joined back together with `\n`. This holds
even if a later line happens to start with `#osc_title:` or another recognized prefix: it is
screen text, not a header, because it isn't part of the contiguous run at the top. This keeps the
rule simple to port (read while it matches; stop at the first line that doesn't) and safe against
a real captured screen that happens to contain literal text starting with `#`.

- `#rule: <id>` - optional. Trimmed of leading/trailing whitespace after the prefix, same as
  `#osc_title` below. The specific rule `Evaluate` is expected to select for this fixture,
  not just the state the filename declares. When present, both readers must assert it: two rules
  that assert the same state but disagree on which one wins would pass a state-only check while
  silently breaking `(state, rule)` parity between the Go and Python evaluators. Omit it only when
  no such assertion is warranted (there is no such fixture in this tree as of this writing - all
  nine carry one).
- `#osc_title: <text>` - optional. Sets `Input.OSCTitle`. Trimmed of leading/trailing whitespace
  after the prefix.
- `#osc_progress: <text>` - optional. Sets `Input.OSCProgress`. Same trimming.
- Everything from the first non-header line onward is `Input.Screen`, joined with `\n`, completely
  unprocessed otherwise (including any leading/trailing whitespace on those lines, and including a
  literal `#` at the start of one of them).

## Reference implementation (for the Python port)

```
read lines from the top of the file
while the current line starts with "#rule:", "#osc_title:", or "#osc_progress:":
    record it as a header (extracting the value after the prefix, trimmed)
    advance to the next line
the current line and everything after it, joined with "\n", is the screen
```

`internal/detect/manifests_test.go`'s `splitFixtureHeaders`, `inputFromFixture`, and
`fixtureExpectedRule` are the Go implementation of exactly this.

## Why this exists (not a filled-in fixture, this file)

A fixture must come from a screen someone actually saw, or from a screen hand-built directly
against a rule's own declared matchers (see `unfixtured.txt`'s header comment, and
`internal/detect/README.md`). This file documents the *format* those fixtures are written in, so
a second reader (plan 03's Python evaluator) can parse the same files the same way without
guessing at header syntax from the nine examples alone.
