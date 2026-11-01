# Start here

openDaisugi runs your coding agents side by side in one terminal, and shows which one needs you.
The warden watches each tool call. With enforce on, it also stops calls outside your envelope.

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
cd openDaisugi && scripts/install.sh && daisugi install
```

- `install.sh` checks your tools first and names any that are missing. Then it builds, and puts
  `coppice`, `sprig` and `daisugi` in `~/.local/bin`. The first build compiles Z3. It happens once, and later builds reuse it.
  Z3 is the slow step. The build uses as many jobs as your cores and free memory allow; set `DAISUGI_NATIVE_JOBS` to change that. It prints one line per step and keeps the compiler output in a log file
  (it prints the path). If a step fails, it shows the last 30 lines of that log.
- Voice engines are not built. Add them later with `scripts/install.sh --voice`.
- `daisugi install` asks before it writes. It writes five things for Claude Code:
  - the warden hook in `~/.claude/settings.json`, watching only: it logs calls and blocks none;
  - a capture hook in `~/.claude/settings.json`, which records tool calls for later study;
  - the `opendaisugi` MCP server in `~/.claude.json`;
  - the `opendaisugi-checklist` skill, copied into `~/.claude/skills` for Claude Code (Codex: `~/.agents/skills`);
  - a short note on pathways, appended to `~/.claude/CLAUDE.md`.

  Add `--dry-run` to see the list for your machine. Codex gets four of the five, in its own files (no capture hook).
- With no envelope, audit mode allows every call and logs it as "would deny". Run
  `daisugi warden init` once, in your workspace directory, to register a starter envelope. Then
  `daisugi warden report` shows what enforce mode would block.
- To make the warden block, run `daisugi install --enforce`. It needs an envelope first.
- Make sure `~/.local/bin` is on your PATH.
- Remove it: `daisugi install --uninstall` reverses all five writes. A file or directory that
  install made and that is now empty goes too, so the config is as install found it. The
  `.bak<number>` backups stay: each run makes one for each file it changes. Then delete the
  three files from `~/.local/bin`.

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

> **Sandbox.** In coppice the fence is on by default while the warden is on. The kernel (Landlock)
> keeps the agent out of `~/.ssh`, the rest of your home and the memory stores. A plain shell pane
> stays bare. See the rules with `daisugi fence --print`. Outside coppice, run
> `daisugi fence -- claude`. Set `fence = "off"` in `~/.config/coppice/coppice.toml` to turn it off;
> the status commands and the floor then say that files are not confined. One limit: checkpoints
> are off inside the fence. Details: [fence](how-to/fence.md).

> **Models.** Your agent uses its own model and account. The steward, the chat that watches
> your agents, can use any model: [steward on any model](how-to/steward-any-model.md). To use
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

- **Talk to the steward.** Set `[steward] mode = "chat"` in `coppice.toml`, restart coppice,
  press `ctrl-t` and type. See [steward on any model](how-to/steward-any-model.md).
- **Import old chats.** `coppice steward import --dry-run` counts them and reads nothing else.
  See [import old chats](how-to/import-old-chats.md).
- **See the Garden.** Press `g` in the web page. `Save picture` writes a PNG, and `Hide words`
  leaves out names so you can share it.
- **Turn on enforce.** Read [the warden guide](how-to/warden.md). Audit mode shows what enforce
  would block.

## After the rename (one time)

The parts have new names: the gate is the warden, the gateway is the router,
the foreman is the steward, and the chat memory is Pando R2. Commands, config
keys and environment variables changed with them. There are no aliases. Do
this once:

1. Stop coppice (`coppice server stop`, with your usual `--socket` if you set
   one) and close any `daisugi start` window.
2. Build and install the new binaries: `scripts/install.sh --replace-daisugi`.
   Until step 3, your old hook calls a verb that no longer exists. It blocks
   every call and says "daisugi gate is now daisugi warden; run daisugi
   install". A Claude Code session that is open now will see that line. It is
   the expected state for a few minutes.
3. Run `daisugi install` (add `--enforce` if you had it, `--router` if you used
   the gateway). Read the plan first with `--dry-run`. It prints one line for
   each thing it moves: `~/.opendaisugi/gate` becomes `~/.opendaisugi/warden`,
   `~/.opendaisugi/gateway` becomes `~/.opendaisugi/router`, the old keys in
   `config.yaml` get their new names, the old hook line in
   `~/.claude/settings.json` is replaced (its mode stays, and its `--root`
   follows the moved directory), and an old pi or
   OpenCode extension is moved under `~/.opendaisugi/moved-aside/`. Nothing is
   deleted. If it refuses, it says why on one line; fix that and run it again.
   If an envelope allows `opendaisugi/delegate`, it names the envelope: add
   `opendaisugi/spawn` there yourself, since the tool was renamed.
4. Edit `coppice.toml` by hand: `[foreman]` is now `[steward]`, and
   `gateway = ` is now `router = `. coppice refuses to start until you do and
   names the key.
5. Start coppice. The steward's log stays where it was (`foreman/` beside the
   coppice data dir); ADR-0023 moves it into daisugi later, in one step. If
   the floor had a named steward, name it again with `floor talk` or
   `coppice task set-steward`; the old `foreman.json` is left in place.
6. Check: `daisugi warden status` shows your mode and no stale key (it names
   an old directory or an old config key if one is left);
   `daisugi router status` answers if you installed the router.

## If something goes wrong

- A saved `claude --settings "$(daisugi gate settings ...)"` line from before the
  rename now fails with the same "run daisugi install" line. Use
  `daisugi warden settings` instead.

- Logs: `server.log` in the coppice data dir. That dir is `$OPENDAISUGI_HOME/coppice` when you
  set that, else `$XDG_DATA_HOME/opendaisugi/coppice` when you set `XDG_DATA_HOME`, else
  `~/.opendaisugi/coppice`. If `~/.opendaisugi/coppice` holds data and the XDG one does not,
  coppice uses the old one and says so.
- The warden blocks too much: `daisugi warden disarm` turns it off. `daisugi warden arm` turns it on.
- Stop everything: `coppice server stop`.
- Save your history: `daisugi backup` and `coppice backup` each write one .tgz. No key, token or credential files go in. Your chat and logs do, so keep the file as private as the data.
- Put it back: `daisugi restore FILE` and `coppice restore FILE` (stop the server first). See [backup](how-to/backup.md).
- Report a bug as a GitHub issue. Give the command, the error and your Linux version. Remove
  chat text, paths with your name, and keys from logs first.
