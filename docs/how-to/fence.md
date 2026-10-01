# Fence any harness with Landlock

*How to run an agent harness so that the kernel, not only the gate, keeps it
out of your keys and your other files. One command, no daemon, no container.*

The gate reads the text of a tool call. It cannot stop a program that the
call starts. `cat ~/.ssh/id_ed25519`, a one-line Python `rmtree` of a
directory outside the workspace, or a Python line that writes the gate's own
`DISARMED` file all pass a text check. `daisugi fence` closes that gap. It
applies a [Landlock](https://docs.kernel.org/userspace-api/landlock.html)
ruleset to itself, sets `no_new_privs`, then runs the harness. Every process
the harness starts inherits the ruleset and none can lift it.

```sh
daisugi fence -- claude            # the harness's own directory is allowed
daisugi fence --workspace ~/proj -- codex
daisugi fence --print              # the rules it would apply; runs nothing
daisugi fence --print --json
```

It works with any harness, because it wraps the process and reads nothing of
the harness's protocol. Claude Code, Codex, pi, OpenCode and sprig have a
built-in entry for their own directories (`~/.claude`, `~/.codex`, `~/.pi`
and so on). Any other command gets the workspace and the system.

## What the harness can reach

| Path | Access |
|---|---|
| The workspace (the current directory, or `--workspace`) and each `--workdir` | read, write, run |
| A private temp dir made for this run (`$TMPDIR` in the harness) | read, write, run |
| The harness's own config directory | read only |
| The parts of it the harness writes (sessions, logs, its credentials file) | read, write |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/tty`, `/dev/ptmx`, `/dev/pts` | read, write |
| `/usr /bin /sbin /lib* /etc /opt /proc /sys` | read, run |
| Each directory on `PATH` (the entry itself, not its parent), the harness's binary directory and this binary's directory | read, run |
| The gate root and `config.yaml` in each data dir | read only; `gate.sock` connects |
| The coppice socket's directory (`--coppice-socket`) | connect only |
| The paths in the gate's envelope (`file_read`, `file_write`) | read, or read and write |
| `--allow PATH` (read) and `--allow PATH:rw` (read and write) | as named |
| Everything else under HOME, `/tmp`, other unix sockets | none |

Never reachable unless you name it with `--allow`: `~/.ssh`, `~/.gnupg`,
`~/.aws`, `~/.azure`, `~/.kube`, `~/.docker`, `~/.config/gh`, browser and
editor profiles (`~/.mozilla`, `~/.config/google-chrome`, `~/.config/Code`),
the cloud, package-manager and model token files (`~/.ollama`,
`~/.cache/huggingface/token`, `~/.config/tailscale`), and coppice's own
directories. The list is `SECRETS` in `opendaisugi/fence.py`. Only `--allow`
opens one: a workspace, work dir, temp dir or harness path that is, holds or
is inside a secret is refused, link or not. So `daisugi fence` in your home
directory, or in a workspace that is a link to `~/.ssh`, says which secret it
would reach and does not start.

The secrets stay shut under the home in `$HOME` and under the home in the
password database, when the two differ. No variable turns the fence off.

The gate's state is never writable. The disarm marker, the envelopes, the
asks, the answers, the audit log, the sessions and `config.yaml` stay read
only, even if the envelope or `--allow` names them.

## The gate hook asks the resident gate

The hook is a child of the harness, so it runs inside the fence, where the
gate's state is read only. It is a thin client: it sends each call, asks
included, to the resident gate (`daisugi gate serve`) on `<gate root>/gate.sock`,
and that server, outside the fence, decides and makes every write. If the
gate is on and nothing answers on `gate.sock`, the hook denies the call.

`daisugi fence` starts the resident gate before it applies the ruleset when
the gate is on and no server answers, and says so on stderr with its pid. It
starts once per session; later runs find it. It starts it with
`--captures-root <gate root>/captures`, so a fenced run's allowed calls are
captured there.

The resident gate takes no path from a request. Its root, and the captures
root (`daisugi gate serve --captures-root DIR`), are fixed when it starts; a
request that names `--root` or `--captures-root`, or a prefix of either, is
denied. A session id must match `[A-Za-z0-9._-]{1,128}` (not `.` or `..`),
or the call is denied. Before it applies the ruleset, the fence writes
`<gate root>/fence-runs/<pid>.json` with its pid, the start time of that pid
and the resolved workspace, and the server finds it from the caller's pid
and its parents: that is how it knows a caller is fenced.

**Checkpoints are off inside the fence.** A `--checkpoints` call from a
fenced session is denied with one line, `checkpoints are off inside the
fence`, and the server runs no git for it. A checkpoint would make the
unfenced server run git on the repo config the fenced agent can write, and
git reads that config again on each call. A call from a caller under no
fence run is denied as before. Outside the fence the hook runs `--ask`,
`--captures-root` and `--checkpoints` calls in-process, as you.

## Claude Code's settings and the workspace

Landlock cannot cut one file out of a granted tree, so the fence keeps the
harness's config read only and grants read and write only on the parts the
harness writes:

| Harness | Read and write |
|---|---|
| claude | `~/.claude/projects/<this workspace>`, `~/.claude/{todos,shell-snapshots,statsig,session-env,file-history,plans,debug,ide}`, `~/.claude/.credentials.json`, `~/.cache/claude`, `~/.local/state/claude` |
| codex | `~/.codex/{sessions,log}`, `~/.codex/history.jsonl`, `~/.codex/auth.json` |
| pi | `~/.pi/agent/sessions` |
| opencode | `~/.local/share/opencode/{storage,log,project}`, `~/.local/share/opencode/auth.json`, `~/.local/state/opencode` |
| sprig | the `--session-dir` coppice passes (a work dir) |

So `~/.claude/settings.json`, hooks, skills, agents, plugins and `CLAUDE.md`
stay read only. So do `~/.claude.json` (it holds `mcpServers` and the
per-project settings a later Claude Code run starts) and OpenCode's
`~/.config/opencode`, its downloaded binaries and its plugin packages in
`~/.cache/opencode`. A harness may warn that it cannot save its state there;
`--allow ~/.claude.json:rw` opens the file when you accept that. Run
OpenCode once outside the fence so it installs its packages. The shell
snapshots and session env Claude Code keeps stay writable: only that
session sources them.

Claude Code loads a project's auto memory (`~/.claude/projects/<project>/memory`)
into a later session in that project, with no fence. So of
`~/.claude/projects` only this workspace's own project dir is writable, named
as Claude Code names it (`/home/me/proj` is `-home-me-proj`): the memory of
every other project is read only. Landlock cannot cut `memory/` out of that
one dir, where the transcript is written too, so this workspace's own memory
stays writable, as its `CLAUDE.md` does. A workspace path that is not ASCII,
or is longer than 200 characters, keeps the whole projects dir writable, and
`--print` says so. OpenCode's snapshot repos (`~/.local/share/opencode/snapshot`)
are read only, since OpenCode runs git on them later: its undo snapshots do
not work inside the fence. `--allow PATH:rw` opens either.

The fence makes a missing read-and-write directory before it
starts, since the harness cannot make it under a read-only parent. It does
not make a missing file: log in once outside the fence first.

Under coppice, a fenced Claude Code pane also starts with
`--settings '<JSON>'`. That document holds the gate hook (by absolute path,
matcher `*`, `onFailure: block`), `disableAllHooks: false` and a
`ConfigChange` hook that exits 2. It outranks the user, project and local
settings files, so a planted file cannot drop the gate hook or switch hooks
off mid-session. The strongest form is a root-owned
`/etc/claude-code/managed-settings.json` with the gate hook and
`allowManagedHooksOnly: true`. That needs root once, and the fence does not
do it for you.

The workspace is granted whole, and Landlock cannot hide the files in it
that run code later: `.mcp.json`, `.envrc`, `.gitmodules`, `opencode.json`,
the `.claude/`, `.codex/` and `.opencode/` trees, and `.git/config` and
`.git/hooks` (or, when `.git` is a file, those of the git dir it names and
its common dir). A fenced agent can plant a git hook that runs later,
outside any fence. Coppice hashes those files when a fenced pane starts and
ends, and again at each new start in that workspace or a dir under it while
the fenced pane runs. A change shows on the pane's rail row (`workspace
config changed: .git/hooks/pre-commit`) and in a note, and the next start of
any pane in that workspace, or in any dir under it, waits: check the files,
then run `coppice pane ack-changed DIR` with the workspace the refusal names.
Panes that run there go on. Your own shell is not held: check before you
run `git` there.

## It refuses rather than runs bare

If the kernel has no usable Landlock, the harness does not start. You get one
message with the reason and the way out. Landlock needs Linux 5.13, and the
fence needs ABI 3 or newer (Linux 6.2), because older ones cannot stop
`truncate`. It also needs ABI 9 to stop unix-socket connects (the session
bus, `ssh-agent`, the tmux server) and ABI 6 to stop signals; below that it
refuses unless you pass `--allow-weak-ipc`, which fences the files only. Your
kernel also needs `landlock` in its `lsm=` list. Check with
`daisugi fence --print`, which shows the ABI.

To run without the fence: `--no-fence`, or leave coppice at its default
(`fence = "off"`).

## Under coppice

The fence is opt-in in coppice: with no `fence` line it starts every pane
bare, as light as a plain harness. Turn it on for all panes with `fence =
"on"`, or for one pane with `coppice pane create --fence on`. With `fence =
"auto"`, coppice fences while the gate is on (any gate root it may use holds
`envelopes/` and no `DISARMED` marker). When the fence is on, it still fails
closed: a pane whose fence cannot be applied does not start. It covers
terminal panes and headless panes, a resume, a fork and a restart, and the
foreman's workers and turns. A plain shell pane is yours
and stays bare. In `coppice.toml`:

```toml
# fence = "off"  # never (the default)
fence = "on"     # always
# fence = "auto" # while the gate is on
```

One pane can choose: `coppice pane create --fence on ...` (or `off`). Only the
operator can turn a fence off: a pane, a plugin or a process coppice cannot place cannot turn a fence
off, and whatever a fenced pane starts (a shell, a custom command, another
agent) is fenced too. A fenced pane can type only into fenced panes. Coppice
pins `HOME`, `TMPDIR` and the XDG and data-dir variables of a fenced pane to
its own, so a request cannot move them. Each run of a fenced pane gets a
fresh private temp dir under `<coppice data>/fence-tmp/`, removed when the
pane ends.

A foreman worker's `daisugi tree run` runs bare, since it writes the worker's
gate root; only its sprig runs under the fence (`DAISUGI_SPRIG_FENCE`), with
the worker's directory and its own session dir writable and the gate root
read only. Coppice also passes `--sprig-fence`, so a `daisugi` that cannot
apply the fence refuses the run and the worker does not start. A worker with
the spawn tool cannot run under the fence.

## What it does not do

- **The network is not limited.** Landlock limits ports, not hosts. A fenced
  harness can still reach any host.
- **chmod, chown, setxattr and utimes are not limited.** No Landlock right
  covers them. An agent can change the mode of a file it cannot open. The
  gate counts a gate root it cannot read as on, and denies an allow whose
  audit line it cannot write, so a chmod cannot switch the gate off without a
  word.
- **The workspace's `.git` and project config are writable** (see above).
- **No checkpoints inside the fence.** A fenced session gets no
  checkpoints and no rewind to one; run without the fence to have them.
- **This workspace's own Claude memory is writable** (see above), like its
  `CLAUDE.md`.
- **A fenced run's captures** go to the captures root the resident gate
  started with, or nowhere: a request cannot name one. `daisugi fence` and
  `daisugi start` both start it with `<gate root>/captures`, so whichever
  starts it first, the captures land there. A server you start by hand with
  no `--captures-root` captures nothing.
- **A standalone run's temp dir is not removed at exit**, because the fence
  execs the harness and is gone. It is removed on the next run: each run
  makes a fresh `<pid>-<random>` dir, and the next `daisugi fence` removes
  the dirs of runs whose process has ended.
- A symlink in `PATH` whose target is outside the grants will not run. Add the
  target's directory with `--allow`.
- A harness that saves a file by writing a temporary file next to it in HOME
  (the usual way to save `~/.claude.json`) cannot create that temporary
  file. Tell us if you see it.

## Cost

The fence adds one exec and a few system calls at start, and nothing per
call. Measured on this box (a fake HOME, `fence -- /bin/true` against
`fence --no-fence -- /bin/true`, median of 300 runs): the fence's own work is
about 1.9 ms in the Go client and 2.3 ms in the Rust client, with the gate
off or on and its server running. Most of it is the private temp dir and
the password lookup. The rest of the start-up is the `daisugi` binary
itself (about 4.6 ms for Go and 2.2 ms for Rust over a bare `/bin/true`).
Starting the resident gate is once per session: the first fenced run with
the gate on and no server takes about 26 ms (Go) or 19 ms (Rust).

## Rules and parity

The rule list comes from one pure function, `opendaisugi.fence.plan`, and the
Go and Rust clients must print the same list for the same input
(`clients/fence_compare.py` runs 88 golden cases through each). Only the
kernel step is in Go and Rust. The rulings are `FN-R-1` to `FN-R-26` in
`clients/ADJUDICATIONS.md`.

Other fills of this slot: Claude Code's `/sandbox` and Codex use bubblewrap,
Cursor uses Landlock and seccomp, and containers. The fence is lighter than a
container, wraps any harness and needs no root.
