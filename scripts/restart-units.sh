#!/usr/bin/env bash
# scripts/restart-units.sh BIN NAME...
# install.sh runs this after it put new binaries in BIN. NAME is each binary
# it replaced (daisugi, coppice, sprig). A user unit that is active and
# runs BIN/NAME still runs the old binary until it restarts, so this
# restarts it and prints one line per unit:
#
#   daisugi-pando.service   runs daisugi pando follow (the memory follower)
#   coppice-server.service  runs the coppice server
#
# The coppice server holds the agents' panes, and a restart ends them. It
# is restarted only when it reports no live pane; otherwise the line says
# how many run and the command to restart it later. A unit that runs some
# other binary is left alone, with a line that says so. With no systemctl,
# or no such unit, this prints nothing.
set -u

bin="${1:?usage: restart-units.sh BIN NAME...}"
shift
command -v systemctl >/dev/null 2>&1 || exit 0

# argv_of UNIT: the first command line of the unit's ExecStart.
argv_of() {
  systemctl --user show -p ExecStart --value "$1" 2>/dev/null |
    sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p' | head -n 1 | sed 's/ *$//'
}

# live_panes ARGV: the live panes of the coppice server that ARGV starts,
# read with its own --socket and --data-dir; "" when it does not answer.
# COPPICE_NO_AUTOSTART: the status call must never start a server of its
# own on the unit's socket (review r9, finding 3).
live_panes() {
  local sock="" data="" prev="" w
  for w in $1; do
    [ "$prev" = --socket ] && sock="$w"
    [ "$prev" = --data-dir ] && data="$w"
    prev="$w"
  done
  local args=()
  [ -n "$sock" ] && args+=(--socket "$sock")
  [ -n "$data" ] && args+=(--data-dir "$data")
  COPPICE_NO_AUTOSTART=1 "$bin/coppice" "${args[@]}" server status --json 2>/dev/null |
    grep -o '"panes_live":[0-9]*' | head -n 1 | cut -d: -f2
}

for name in "$@"; do
  case "$name" in
    daisugi) unit=daisugi-pando.service ;;
    coppice) unit=coppice-server.service ;;
    *) continue ;;
  esac
  systemctl --user is-active --quiet "$unit" 2>/dev/null || continue
  argv="$(argv_of "$unit")"
  exe="${argv%% *}"
  # Both sides resolved: with install.sh --link, BIN/NAME is a link, and
  # coppice server install writes the link's target into ExecStart.
  if [ -z "$exe" ] || [ "$(readlink -f -- "$exe")" != "$(readlink -f -- "$bin/$name")" ]; then
    echo "$unit: not restarted: it runs ${exe:-an unknown binary}, not $bin/$name."
    continue
  fi
  if [ "$unit" = coppice-server.service ]; then
    live="$(live_panes "$argv")"
    if [ -z "$live" ]; then
      echo "$unit: not restarted: its server does not answer, and this starts none. Check it: systemctl --user status $unit"
      continue
    fi
    if [ "$live" -gt 0 ]; then
      echo "$unit: not restarted: agents run in it ($live now), and a restart ends them. When they finish: systemctl --user restart $unit"
      continue
    fi
  fi
  if systemctl --user restart "$unit" >/dev/null 2>&1; then
    echo "restarted $unit: it runs the new $name."
  else
    echo "$unit: the restart failed. Run: systemctl --user restart $unit"
  fi
done
exit 0
