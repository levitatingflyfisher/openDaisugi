#!/usr/bin/env bash
# Fail while an old part name is left in a tracked file or path.
# Old words: gate, gated, gates, gateway, gateways, foreman, foremen (whole word, any case).
# Allowlist: scripts/names-allow.txt. Usage: scripts/names-check.sh [--count]
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
allow=scripts/names-allow.txt
excl=(); prefixes=(); phrases=()
while IFS= read -r line; do
  line="${line%%#*}"
  case "$line" in
    "path "*) p="${line#path }"; p="${p%"${p##*[![:space:]]}"}"; excl+=(":!$p*"); prefixes+=("$p") ;;
    "phrase "*) p="${line#phrase }"; p="${p%"${p##*[![:space:]]}"}"; phrases+=("$p") ;;
  esac
done < <(grep -v '^[[:space:]]*$' "$allow" || true)
words='gate|gated|gates|gateway|gateways|foreman|foremen'
# Underscore, digit and CamelCase joins count as boundaries, so gate_mode and GateState are hits too.
camel="[a-z](Gate|Foreman)|(Gate|Foreman)[A-Z]"
tokens='"delegate"|daisugi-delegate|opendaisugi\.delegate|run_delegate|delegate\.py'
out=$(mktemp); trap 'rm -f "$out"' EXIT
git grep -I -n -i -E "(^|[^A-Za-z])($words)([^A-Za-z]|$)" -- . "${excl[@]}" >>"$out" || true
git grep -I -n -E "$camel" -- . "${excl[@]}" >>"$out" || true
git grep -I -n -E "$tokens" -- . "${excl[@]}" >>"$out" || true
# path names that carry a word (separators count as boundaries)
git ls-files | grep -i -E "(^|[^a-z0-9])($words)([^a-z0-9]|$)" | while IFS= read -r f; do
  skip=0
  for p in "${prefixes[@]}"; do case "$f" in "$p"*) skip=1; break ;; esac; done
  [ "$skip" = 1 ] || echo "$f"
done >>"$out" || true
if [ ${#phrases[@]} -gt 0 ]; then
  pf=$(mktemp); printf '%s\n' "${phrases[@]}" >"$pf"
  grep -v -i -F -f "$pf" "$out" >"$out.2" || true; mv "$out.2" "$out"; rm -f "$pf"
fi
sort -u "$out" -o "$out"
n=$(wc -l <"$out")
if [ "${1:-}" = "--count" ]; then echo "$n"; else cat "$out"; fi
[ "$n" -eq 0 ]
