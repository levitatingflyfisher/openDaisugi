#!/usr/bin/env bash
# A stand-in for `pi --mode rpc`: JSON lines in on stdin, JSON lines out on
# stdout, no model. It answers get_state and switch_session, and plays one
# short turn per prompt:
#
#   a prompt with "ask" in it   opens a confirm dialog and waits for the
#                               extension_ui_response line, then says what
#                               came back
#   "garbage"                   prints a line that is not JSON
#   "bye"                       exits 4 right away
#   anything else               streams "you said: TEXT" as two text
#                               deltas, then one tool call
#
# Every turn starts with agent_start and ends with agent_end and
# agent_settled. COPPICE_PI_ECHO_STDIN, when set, gets every stdin line.
set -uo pipefail
session_file="${COPPICE_PI_SESSION_FILE:-/sessions/pi-1.jsonl}"

field() { # field NAME LINE: the string value of "NAME":"..." in LINE
  printf '%s' "$2" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"
}

while IFS= read -r line; do
  if [ -n "${COPPICE_PI_ECHO_STDIN:-}" ]; then
    printf '%s\n' "$line" >> "$COPPICE_PI_ECHO_STDIN"
  fi
  case "$line" in
    *'"type":"get_state"'*)
      printf '{"type":"response","command":"get_state","success":true,"data":{"sessionId":"pi-s1","sessionFile":"%s","isStreaming":false}}\n' "$session_file"
      ;;
    *'"type":"switch_session"'*)
      session_file=$(field sessionPath "$line")
      printf '{"type":"response","command":"switch_session","success":true}\n'
      ;;
    *'"type":"prompt"'*)
      msg=$(field message "$line")
      printf '{"type":"agent_start"}\n'
      case "$msg" in
        bye) exit 4 ;;
        garbage) printf 'not json\n' ;;
        *ask*)
          printf '{"type":"extension_ui_request","id":"ui-1","method":"confirm","title":"Run the tests?"}\n'
          IFS= read -r answer || exit 0
          if [ -n "${COPPICE_PI_ECHO_STDIN:-}" ]; then
            printf '%s\n' "$answer" >> "$COPPICE_PI_ECHO_STDIN"
          fi
          case "$answer" in
            *'"confirmed":true'*) said=true ;;
            *) said=false ;;
          esac
          printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"confirmed: %s"}}\n' "$said"
          printf '{"type":"message_end"}\n'
          ;;
        *)
          printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"you said: "}}\n'
          printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"%s"}}\n' "$msg"
          printf '{"type":"message_end"}\n'
          printf '{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"z":1.50,"a":"<b>","n":[1e21,0.0000001]}}\n'
          ;;
      esac
      printf '{"type":"agent_end"}\n'
      printf '{"type":"agent_settled"}\n'
      ;;
  esac
done
