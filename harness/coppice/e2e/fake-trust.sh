#!/usr/bin/env bash
# fake-trust.sh SCREEN: a stand-in for claude's first run. It draws the
# recorded folder trust screen from SCREEN (a testdata/screens file, its
# "#rule:" header line dropped), with the cursor on "No, exit" as claude
# 2.1.282 puts it. Up and Down move the cursor. Enter on "Yes, I trust this
# folder" draws a prompt box, which coppice reads as idle; Enter on
# "No, exit", or Esc, ends it, as they end the real claude. No model and no
# network are involved.
set -euo pipefail
screen="${1:?usage: fake-trust.sh SCREEN}"
cursor=0 # 0 is "No, exit", 1 is "Yes, I trust this folder"

draw() {
  printf '\033[2J\033[H'
  sed '1{/^#rule:/d}' "$screen" | awk -v c="$cursor" '
    /No, exit/ { print (c == 0 ? " ❯ No, exit" : "   No, exit"); next }
    /Yes, I trust this folder/ { print (c == 1 ? " ❯ Yes, I trust this folder" : "   Yes, I trust this folder"); next }
    { print }'
}

draw
while IFS= read -rsn1 key; do
  case "$key" in
    "")
      if [ "$cursor" -eq 1 ]; then
        printf '\033[2J\033[H'
        printf '%s\n' '────────────────────' '❯ ' '────────────────────'
        exec sleep 3600
      fi
      exit 1 ;;
    $'\033')
      # An arrow key is ESC [ A or ESC [ B; a lone ESC is Esc.
      if IFS= read -rsn2 -t 0.1 rest; then
        case "$rest" in
          "[A" | "OA") cursor=0 ;;
          "[B" | "OB") cursor=1 ;;
        esac
        draw
      else
        exit 1
      fi ;;
  esac
done
