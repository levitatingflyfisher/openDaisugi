# Start here

openDaisugi runs your coding agents side by side in one terminal, and shows which one needs you.
A gate can check each tool call before it runs.

## Install

You need Linux and these packages first. `install.sh` fetches zig, cmake and a new enough Go
toolchain by itself (no sudo).

- Omarchy and Arch: `sudo pacman -S --needed git base-devel pkgconf go curl python`
- Fedora: `sudo dnf install git gcc gcc-c++ make pkgconf-pkg-config golang curl python3 binutils`
- Debian and Ubuntu: `sudo apt install git build-essential pkg-config golang-go curl python3 binutils xz-utils`
  This needs Debian 13 or Ubuntu 24.04 or later. Older ones ship a Go below 1.21, which cannot
  fetch the new one. Install a newer Go from <https://go.dev/dl> first.

```sh
git clone https://github.com/levitatingflyfisher/openDaisugi.git
cd openDaisugi && scripts/install.sh && daisugi install --gate
```

- `install.sh` checks your tools first and names any that are missing. Then it builds, and puts
  `coppice`, `sprig` and `daisugi` in `~/.local/bin`. The first build takes about 45 min on a 4-core machine (about 9 min on 12 cores), once. Later builds reuse it.
  Z3 is the slow step. The build uses as many jobs as your cores and free memory allow; set `DAISUGI_NATIVE_JOBS` to change that. It prints one line per step and keeps the compiler output in a log file
  (it prints the path). If a step fails, it shows the last 30 lines of that log.
- Voice engines are not built. Add them later with `scripts/install.sh --voice`.
- `daisugi install --gate` asks before it writes. It writes five things for Claude Code:
  - the gate hook in `~/.claude/settings.json`, in audit mode: it logs calls and blocks none;
  - a capture hook in `~/.claude/settings.json`, which records tool calls for later study;
  - the `opendaisugi` MCP server in `~/.claude.json`;
  - the `opendaisugi-checklist` skill, copied into `~/.agents/skills` (or `~/.claude/skills` if you already have that one);
  - a short note on pathways, appended to `~/.claude/CLAUDE.md`.

  Add `--dry-run` to see the list for your machine. Codex gets four of the five, in its own files (no capture hook).
- With no envelope, audit mode allows every call and logs it as "would deny". Run
  `daisugi gate init` once, in your workspace directory, to register a starter envelope. Then
  `daisugi gate report` shows what enforce mode would block.
- Make sure `~/.local/bin` is on your PATH.
- Remove it: `daisugi install --uninstall` reverses all five writes. Then delete the three files
  from `~/.local/bin`.

## First run

Type `coppice`. It finds your agent (`claude`, `codex`, `opencode`, `pi`) and asks once if
there are several. Press Enter to open it in the current directory. Now you type in the agent.

| Key | What it does |
|---|---|
| `Enter` | open the agent under the cursor in a window and type there |
| `ctrl-space` | give the keys back to the list of agents |
| `n` | start another agent in the same directory |
| `shift-tab` | jump to the next agent that needs you |
| `g` | in the web page: the Garden, a picture of your agents |

On your phone: `coppice web serve`, then read [the phone guide](how-to/phone.md).

> **Sandbox (optional).** The fence is off by default. When on, the kernel (Landlock) keeps the
> agent out of `~/.ssh` and the rest of your home. See the rules with `daisugi fence --print`.
> Turn it on for all panes with `fence = "on"` in `~/.config/coppice/coppice.toml`, or for one
> run with `daisugi fence -- claude`. One limit: checkpoints are off inside the fence.
> Details: [fence](how-to/fence.md).

> **Models.** Your agent uses its own model and account. The foreman, the chat that watches
> your agents, can use any model: [foreman on any model](how-to/foreman-any-model.md). To use
> a local model on another machine over Tailscale, put this in `~/.config/coppice/pools.toml`:
>
> ```toml
> [pool.desk]                 # a profile name from your agent profiles
> model = "llama3.3:70b"
>
> [[pool.desk.entries]]
> name = "gpu"
> base_url = "http://gpu.your-tailnet.ts.net:11434/v1"
> ```
>
> A model never changes on its own. A pool serves one model, and when a machine is down, work
> waits or moves to another machine with the same model.

## Try next

- **Talk to the foreman.** Set `[foreman] mode = "chat"` in `coppice.toml`, restart coppice,
  press `ctrl-t` and type. See [foreman on any model](how-to/foreman-any-model.md).
- **Import old chats.** `coppice foreman import --dry-run` counts them and reads nothing else.
  See [import old chats](how-to/import-old-chats.md).
- **See the Garden.** Press `g` in the web page. `Save picture` writes a PNG, and `Hide words`
  leaves out names so you can share it.
- **Enforce the gate.** Read [the gate guide](how-to/gate.md). Audit mode shows what enforce
  would block.

## If something goes wrong

- Logs: `server.log` in the coppice data dir. The rule never asks whether `~/.opendaisugi`
  exists. It is `$OPENDAISUGI_HOME/coppice` when you set that, else
  `$XDG_DATA_HOME/opendaisugi/coppice` when you set `XDG_DATA_HOME`, else
  `~/.opendaisugi/coppice`. Coppice from before keeps its place: if `~/.opendaisugi/coppice`
  holds data and the XDG one does not, coppice uses the old one and says so in one line.
- Gate blocks too much: `daisugi gate disarm` turns the gate off. `daisugi gate arm` turns it on.
- Stop everything: `coppice server stop`.
- Save your history: `daisugi backup` and `coppice backup` each write one .tgz. No key, token or credential files go in. Your chat and logs do, so keep the file as private as the data.
- Put it back: `daisugi restore FILE` and `coppice restore FILE` (stop the server first). See [backup](how-to/backup.md).
- Report a bug as a GitHub issue. Give the command, the error and your Linux version. Remove
  chat text, paths with your name, and keys from logs first.
