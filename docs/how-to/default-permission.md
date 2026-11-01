# Let the steward work in a known project without a card

Each project gets one default permission. The steward grants it to a known
project by itself, the first time it spawns a worker there. You see a card
only when the steward asks for more. See
[ADR-0030](../adr/0030-one-default-permission.md).

## Which projects are known

A project is known when its dir is one of the trusted roots or sits under
one. The trusted roots are:

- each pinned project (`projects` in `coppice.toml`), and
- each dir in `[steward] trusted_roots`. When you set none, it is `~/code`.

```toml
[steward]
trusted_roots = ["~/code", "/data/src"]
```

A root must be an absolute path or start with `~/`. coppice refuses `/` and
your home dir: they would make every dir known. Symlinks are resolved, and a
dir that only starts with the same letters (`~/code2`) is not under `~/code`.

## What the default allows

The default envelope is written to `<config dir>/envelopes/<project>.json`,
the same file an approved card writes. It is marked `"granted_by":
"default"`. A file from an approved card says `"owner"`.

| Part | The default |
|---|---|
| Read and write | the project dir and everything under it |
| Shell | any command named by one word (`git`, `flutter`, `make`), and a script in the project's top dir (`./gradlew`). Compound commands (`a && b`) are checked part by part |
| Network | off. This covers the warden's network steps only: a shell command that fetches (`flutter pub get`) is not checked for it |
| Budget | none: no token or turn limit |
| Step cap | 30 s for each shell step (`max_execution_time_s`) |
| Run cap | 1800 s for a worker's whole run (`[steward] worker_max_run_s`). A lead has none |
| Stakes | `low` |

A task with a long build or test loop is a lead, not a worker: the steward
starts it with `long_running: true`. A lead runs in a pane of its own, with
no step cap.

## The deny list

These verbs publish or spend. The default denies each one, and so does every
envelope the steward proposes, unless an approved card allowed it:

| Verb | What it does |
|---|---|
| `git push` | publishes commits |
| `gh release` | makes or edits a release |
| `gh pr merge` | merges a pull request |
| `flutter build appbundle` | builds a store upload |
| `fastlane` | deploys to a store |
| `stripe` | moves money |
| `npm publish` | publishes a package |
| `cargo publish` | publishes a crate |
| `gh repo delete` | deletes a repo |
| `gh workflow run` | starts a workflow, often a deploy |
| `gh api` that writes a release, a ref, a file, a merge, a dispatch or a Pages site, deletes anything, or sends a GraphQL mutation | the same, through the API. A write method (`-X POST`, `-XPOST`, `--method put`) or a field flag (`-f`, `-F`, `--field`, `--raw-field`, `--input`) makes it a write. A read (`gh api repos/o/r/releases`) is allowed |
| `pnpm publish`, `yarn publish` | publish a package |

The list lives in one place, `clients/go/envdefault`. Each verb is an
invariant of the envelope (`deny_git_push` and so on). The warden checks
the shell command against it and names the invariant when it denies. A
tool's name counts wherever it starts a word, so a wrapper in front of it
(`xargs`, `timeout`, `nice`, `ionice`, `flock`, `stdbuf`, `watch`, `env`,
`sudo`, `bash -c '...'`) does not hide it. Options before the verb count
too (`git -C dir push`), and so do quotes, backslashes and a
backslash-newline inside a word (`git "push"`, `git pu''sh`). The price is
a false deny on a command that only names a verb, such as `echo git push`.

It is a guard rail, not a wall. It reads the command text, so it does not
see:

- a git alias (`git p` with `p = push` in `.git/config`),
- a script or a Makefile target that runs a verb inside itself,
- a symlink or a copy of a tool under another name,
- a tool called by its full path in an envelope that allows that path,
- a shell variable, expansion or brace that spells the verb (`git $p`,
  `git $'push'`, `git pus{h,}`), or a verb fed on stdin (`echo push | xargs git`).

The wall is the kernel fence. It keeps a worker out of your keys and your
other dirs (see [fence.md](fence.md)), and a push with no credentials
fails. The fence is on only when the warden is on and the kernel has
Landlock ABI 9, or when `fence = "on"` is set. So the default is granted
with no card only when the worker will run fenced. With the fence off, a
worker in a known project cards once, and the card says the fence is off.

## When a card appears

The steward proposes an envelope with `coppice envelope propose`. A
proposal that fits the default of a known project is written at once, with
no card. A card appears when the proposal asks for more:

- the project is outside the trusted roots,
- the fence is off, so its workers would run with your keys in reach,
- it reads or writes outside the project dir, or names a relative path, a
  path with `..`, or a link that leads out of the project,
- it sets a budget,
- it turns on the network or names hosts,
- it asks for a denied verb (`--allow "git push"`),
- it names a shell command with a path (`/usr/bin/make`), or a denied tool,
- it adds MCP tools or custom steps,
- it raises the step cap or the output cap,
- it turns off the interpreter warning (`shell_interpreter_policy: allow`),
- it brings invariants of its own (the deny list is always the default's),
- it would replace an envelope you approved or wrote.

A proposal that fits gets the default envelope itself written, never its
own text.

The card lists each of these that holds, and the verbs that stay denied.
See [needs-you.md](needs-you.md).

A worker spawned into a project outside the trusted roots, with no envelope,
is refused with one line, and you get a card for the default envelope.
Approve it, and the steward spawns again.

## A budget, when you set one

An envelope with no budget asks for none. When you set `tokens` or `turns`
in the file, each worker asks the project's root for what it has left. A
spent budget fails the worker with one line that names the file to raise.

## A changed envelope

Each write of an envelope file has its own root in the tree, named by a
hash of the file and its time of change. When you approve a new envelope,
or edit the file, the next worker runs under the new one, with a fresh
budget. Workers already running keep the old one. Old roots stay listed in
`daisugi tree status`.
