# Start here

openDaisugi runs your coding agents side by side in one terminal, and shows which one needs you.
A gate can check each tool call before it runs.

## Install

You need Linux, git and a C toolchain. On Omarchy, run `sudo pacman -S pkgconf` first.

```sh
git clone https://github.com/levitatingflyfisher/openDaisugi.git
cd openDaisugi && scripts/install.sh && daisugi install --gate
```

- `install.sh` checks your tools first and names any that are missing. Then it builds, and puts
  `coppice`, `sprig` and `daisugi` in `~/.local/bin`. The first build takes a while.
- `daisugi install --gate` adds a hook to Claude Code or Codex. It starts in audit mode: it logs
  calls and blocks none. Add `--dry-run` to see what it writes.
- Make sure `~/.local/bin` is on your PATH.
- Remove it: `daisugi install --uninstall`, then delete the three files from `~/.local/bin`.

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

- Logs: `server.log` in the coppice data dir, `~/.local/share/opendaisugi/coppice/` by default.
  If `~/.opendaisugi` exists, it is `~/.opendaisugi/coppice/`.
- Gate blocks too much: `daisugi gate disarm` turns the gate off. `daisugi gate arm` turns it on.
- Stop everything: `coppice server stop`.
- Report a bug as a GitHub issue. Give the command, the error and your Linux version. Remove
  chat text, paths with your name, and keys from logs first.
