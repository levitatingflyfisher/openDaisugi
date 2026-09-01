# tmux control-mode transcripts

Each `.txt` file here is what `tmux -C attach` printed on stdout, recorded
from tmux 3.7b on a private server socket. `internal/tmuxmirror` parses them
in its tests. No test runs tmux.

- `lifecycle.txt`: list-windows, new-window, set-option, rename-window,
  kill-window, and a command tmux does not know, which ends in `%error`.
- `window-close.txt`: two windows whose commands exit, with `%output` from
  one of them. tmux 3.7b reports each close as `%unlinked-window-close`.
