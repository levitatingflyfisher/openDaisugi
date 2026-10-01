#!/usr/bin/env bash
# clients/native/parakeet-cli/check-binary.sh [--rules] BINARY...
#
# Fails (exit 1) when a built program carries code it must not: the
# CrispASR model ports and tables this build leaves out (to keep it small,
# offline and auditable), CrispASR's model download code, or network code. native.sh --parakeet
# runs it on parakeet-cli and parakeet-quantize before it installs them.
#
# - strings: no name of a left-out model or table, and no URL scheme.
# - nm: no symbol of a left-out part, and no socket, DNS or curl call.
# - ldd: only the C runtime's own libraries.
#
# --rules skips the strings check of model names, and only that, for
# parakeet-quantize: CrispASR's quantizer keeps per-model rules (which
# tensors of which architecture stay F16), and those rules name other
# models' tensors as plain strings. No code of those models is in it; the
# nm check still runs, as do the URL, network and library checks.
set -euo pipefail

rules=0
if [ "${1:-}" = "--rules" ]; then rules=1; shift; fi
[ "$#" -gt 0 ] || { echo "usage: check-binary.sh [--rules] BINARY..." >&2; exit 2; }
for tool in strings nm ldd; do
  command -v "$tool" >/dev/null 2>&1 || { echo "check-binary.sh: $tool is not on PATH" >&2; exit 1; }
done

# Names of the left-out code: CrispASR's ports of the other models, the
# OmniVoice tables, the pinyin tables, k2-fsa and Kaldi, and CrispASR's
# Hugging Face download cache.
names='omnivoice|pinyin|kaldi|k2-fsa|k2fsa|xiaomi|qwen|firered|funasr|paraformer|sensevoice|cosyvoice|glm_asr|glm-asr|glmasr|mimo_|moss_|indextts|crispasr_cache|libcurl|curl_easy'
# Calls that reach a network.
calls='^(socket|connect|getaddrinfo|gethostbyname|gethostbyname_r|curl_.*|SSL_.*|send|sendto|recv|recvfrom)$'
allowed_libs='^(linux-vdso\.so|libc\.so|libm\.so|ld-linux[-a-z0-9_.]*\.so|libpthread\.so|libdl\.so|librt\.so)'

fail=0
for bin in "$@"; do
  [ -f "$bin" ] || { echo "check-binary.sh: $bin is not a file" >&2; exit 1; }
  pat="$names|https?://|ftp://|huggingface\.co|hf\.co/"
  [ "$rules" -eq 0 ] || pat='https?://|ftp://|huggingface\.co|hf\.co/'
  if hits="$(strings -a "$bin" | grep -Eio "$pat" | sort -u)" && [ -n "$hits" ]; then
    echo "check-binary.sh: $bin carries strings it must not: $(echo "$hits" | tr '\n' ' ')" >&2
    fail=1
  fi
  syms="$( { nm -a "$bin" 2>/dev/null || true; nm -D "$bin" 2>/dev/null || true; } | awk '{print $NF}' | sort -u)"
  if hits="$(echo "$syms" | grep -Ei "$names")" && [ -n "$hits" ]; then
    echo "check-binary.sh: $bin carries symbols it must not: $(echo "$hits" | head -5 | tr '\n' ' ')" >&2
    fail=1
  fi
  if hits="$(echo "$syms" | sed 's/@.*//' | grep -E "$calls")" && [ -n "$hits" ]; then
    echo "check-binary.sh: $bin calls the network: $(echo "$hits" | tr '\n' ' ')" >&2
    fail=1
  fi
  while read -r lib _; do
    [ -n "$lib" ] || continue
    if ! echo "$(basename "$lib")" | grep -Eq "$allowed_libs"; then
      echo "check-binary.sh: $bin links $lib" >&2
      fail=1
    fi
  done < <(ldd "$bin" 2>/dev/null | sed 's/^[[:space:]]*//' | grep -v 'statically linked' || true)
done
[ "$fail" -eq 0 ] || exit 1
echo "checked: $*"
