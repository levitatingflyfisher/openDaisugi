# coppice in Neovim

This plugin shows the coppice roster inside Neovim. It is a client you
install in Neovim. The coppice binary does not carry it, and the server
does not start it.

## Install

Put this directory on your runtime path. With lazy.nvim:

```lua
{
  dir = "/path/to/openDaisugi/harness/coppice/plugins/nvim",
  config = function()
    require("coppice").setup({})
  end,
}
```

Without a plugin manager, add the directory to `runtimepath` and call
`require("coppice").setup({})` from your `init.lua`.

`setup` takes two optional keys:

| key | default | meaning |
|---|---|---|
| `socket` | `$COPPICE_SOCKET`, else `$XDG_RUNTIME_DIR/coppice/server.sock`, else `~/.opendaisugi/coppice/server.sock` | The server socket. |
| `coppice` | `coppice` | The program that `<CR>` runs to attach. |

The plugin needs Neovim 0.10 or newer and a running coppice server. It
does not start a server.

## Use

`:Coppice` opens the roster in a floating window. The panes that need you
come first, then the working ones, then the idle ones.

| key | what it does |
|---|---|
| `<Space>` | Shows the pane's screen in a split. The split is a copy and takes no input. |
| `<CR>` | Opens a terminal buffer in a new tab that runs `coppice --socket <socket> attach <pane>`. Your keys go to the pane. The attach key that leaves, ctrl-space by default, ends the terminal job. |
| `r` | Reads the roster again. |
| `<Esc>`, `q` | Close the roster. |

A full attach sizes the pane to the terminal buffer, the same as an
attach from any other terminal. The last attach to send a size wins.

## What it may do

The plugin runs `pane.list` and `pane.read` on the socket, and `<CR>`
runs `coppice attach`. It never runs `agent.allow` or `agent.deny`. Answer
an ask from the floor, the phone, or `coppice agent allow`.

The server places each connection by its process ancestry. Neovim that
runs inside a coppice pane is placed as that pane, so its connection can
never allow or deny, whatever this plugin sends.

## Test

`lua/coppice/roster.lua` and `lua/coppice/wire.lua` are the pure core.
They hold no vim call. The spec runs them in a headless Neovim:

```
cd harness/coppice/plugins/nvim
nvim --headless --clean -c "luafile tests/roster_spec.lua"
```

`go test ./internal/plugins` runs the same spec when `nvim` is on `PATH`.
Without it, the test skips and says the spec did not run. The rest of the
plugin, the socket and the windows, has no automated test. Check it by
hand.
