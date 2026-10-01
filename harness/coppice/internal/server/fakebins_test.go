package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fakes of the chat mode. No test runs a real claude, sprig or
// daisugi: each is a shell script in the test's scratch dir that logs its
// argv and the size of each descriptor it was given, never a value.

// fakeSprigScript answers a one-shot summary call. FAKE_MODE picks the
// answer: fit (default), longthenfit (too long until the prompt says "too
// long:"), alwayslong, or slow (sleeps 30 s). FAKE_MS holds each call for
// that many milliseconds. It logs "start"/"end" lines to FAKE_INFLIGHT and
// its argv and the size of descriptor 3 to FAKE_LOG.
const fakeSprigScript = `#!/bin/sh
{ printf 'argv:'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >> "$FAKE_LOG"
case " $* " in *" --key-fd "*) has_key=1 ;; *) has_key= ;; esac
if [ -n "$has_key" ]; then
  key=$(cat <&3)
  echo "fd3: ${#key} bytes" >> "$FAKE_LOG"
  sum=$(printf %s "$key" | sha256sum | cut -d' ' -f1)
  if [ "$sum" = "$FAKE_SENTINEL_SHA" ]; then echo "key: matches" >> "$FAKE_LOG"; else echo "key: differs" >> "$FAKE_LOG"; fi
fi
prompt=$(cat)
echo "start $$" >> "$FAKE_INFLIGHT"
if [ -n "$FAKE_MS" ]; then sleep "0.$(printf '%03d' "$FAKE_MS")"; fi
long=$(printf 'L%.0s' $(seq 1 700))
case "${FAKE_MODE:-fit}" in
  alwayslong) answer="$long" ;;
  longthenfit) case "$prompt" in *"too long:"*) answer="a fitting line" ;; *) answer="$long" ;; esac ;;
  slow) sleep 30; answer="late" ;;
  *) answer="a short summary" ;;
esac
echo "end $$" >> "$FAKE_INFLIGHT"
printf '{"answer":"%s","model":"fake","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":1}}\n' "$answer"
`

// fakeDaisugiScript answers agent-profiles bundle with a table that holds the
// profile desk and its key, FAKE_SENTINEL, on the descriptor it is given
// (FAKE_DESK_WIRE and FAKE_DESK_BASE set the desk's wire and base URL), and
// the profile free, which has no key.
const fakeDaisugiScript = `#!/bin/sh
{ printf 'daisugi:'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >> "$FAKE_LOG"
if [ "$1 $2" = "agent-profiles bundle" ]; then
  if [ -n "$FAKE_BUNDLE_GRANDCHILD" ]; then sleep 30 >/dev/null 2>&1 & fi
  if [ -n "$FAKE_BUNDLE_HANG" ]; then sleep 30; fi
  printf '{"default":{"wire":"claude-code","base_url":null,"model":null,"key_env":null,"key":null},"desk":{"wire":"%s","base_url":"%s","model":"desk-model","key_env":"DESK_API_KEY","key":"%s"},"free":{"wire":"openai","base_url":"%s","model":"free-model","key_env":null,"key":null},"cc":{"wire":"claude-code","base_url":null,"model":"sonnet","key_env":null,"key":null}}' "${FAKE_DESK_WIRE:-openai}" "${FAKE_DESK_BASE:-http://127.0.0.1:1/v1}" "$FAKE_SENTINEL" "${FAKE_FREE_BASE:-http://127.0.0.1:2/v1}" >&3
  exit 0
fi
if [ "$1 $2" = "tree root" ]; then
  echo "tree root pwd: $(pwd)" >> "$FAKE_LOG"
  echo "tree root env COPPICE_DATA_DIR=$COPPICE_DATA_DIR" >> "$FAKE_LOG"
  exit 0
fi
if [ "$1 $2" = "tree run" ]; then
  echo "tree run pwd: $(pwd)" >> "$FAKE_LOG"
  in=$(cat)
  echo "stdin: $in" >> "$FAKE_LOG"
  case " $* " in *" --keys-fd "*) has_keys=1 ;; *) has_keys= ;; esac
  if [ -n "$has_keys" ]; then
    bundle=$(cat <&3)
    echo "fd3: ${#bundle} bytes" >> "$FAKE_LOG"
    case "$bundle" in *"$FAKE_SENTINEL"*) echo "bundle: has the key" >> "$FAKE_LOG" ;; *) echo "bundle: lacks the key" >> "$FAKE_LOG" ;; esac
    echo "bundle urls: $(printf %s "$bundle" | grep -o '"base_url":"[^"]*"' | tr '\n' ' ')" >> "$FAKE_LOG"
    echo "run ids: $(printf %s "$*" | grep -o -e '--profile=[^ ]*' -e '--model=[^ ]*' | tr '\n' ' ')" >> "$FAKE_LOG"
  fi
  for u in $FAKE_FAIL_URL; do
    case "$bundle" in *"\"base_url\":\"$u\""*) echo "$FAKE_FAIL_ERR" >&2; exit 1 ;; esac
  done
  trap 'echo "tree run got TERM $$" >> "$FAKE_INFLIGHT"; exit 143' TERM
  echo "tree run start $$" >> "$FAKE_INFLIGHT"
  echo "tree run env COPPICE_DATA_DIR=$COPPICE_DATA_DIR" >> "$FAKE_LOG"
  if [ -n "$DAISUGI_SPRIG_FENCE" ]; then echo "tree run env DAISUGI_SPRIG_FENCE=$DAISUGI_SPRIG_FENCE" >> "$FAKE_LOG"; fi
  if [ -n "$FAKE_TREE_GRANDCHILD" ]; then setsid sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$FAKE_TREE_GRANDCHILD"; fi
  if [ -n "$FAKE_TREE_MS" ]; then sleep "$(awk "BEGIN{print $FAKE_TREE_MS/1000}")" & wait $!; fi
  echo "tree run end $$" >> "$FAKE_INFLIGHT"
  if [ -n "$FAKE_TREE_ERR_IF" ]; then case "$in" in *"$FAKE_TREE_ERR_IF"*) ;; *) FAKE_TREE_ERR=; FAKE_TREE_EXIT=0 ;; esac; fi
  if [ -n "$FAKE_TREE_STATUS" ]; then printf '%s\n' "$FAKE_TREE_STATUS"; fi
  if [ -n "$FAKE_TREE_ERR" ]; then echo "$FAKE_TREE_ERR" >&2; fi
  exit "${FAKE_TREE_EXIT:-0}"
fi
echo "unknown verb" >&2
exit 2
`

// setSentinel sets the key the fake daisugi puts in the bundle, and the hash
// the fakes compare the key they read on descriptor 3 against, since the key
// itself is not in their environment.
func setSentinel(t *testing.T, key string) {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	t.Setenv("FAKE_SENTINEL", key)
	t.Setenv("FAKE_SENTINEL_SHA", hex.EncodeToString(sum[:]))
}

func init() {
	// The fakes read their script from FAKE_* variables and the helper mode
	// from COPPICE_FOREMAN_HELPER; the key itself never goes on.
	sprigEnvKeep = func(name string) bool {
		return name != "FAKE_SENTINEL" && strings.HasPrefix(name, "FAKE_") || name == "COPPICE_FOREMAN_HELPER"
	}
}

// fakeEnv is a test's scratch: its fake binaries and logs.
type fakeEnv struct {
	dir      string
	log      string
	inflight string
}

// newFakeEnv makes the scratch dir, writes the fakes, and points the
// environment at it: HOME and every XDG dir and OPENDAISUGI_HOME inside it,
// and COPPICE_CLAUDE_BIN, COPPICE_SPRIG_BIN and COPPICE_DAISUGI_BIN at the
// fakes. It fails the test when a binary the server would start is outside
// it, since the claude binary falls back to the real one.
func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := &fakeEnv{dir: dir, log: filepath.Join(dir, "fake.log"), inflight: filepath.Join(dir, "inflight.log")}
	for k, sub := range map[string]string{
		"HOME": "home", "XDG_STATE_HOME": "state", "XDG_DATA_HOME": "data", "XDG_CONFIG_HOME": "config",
		"OPENDAISUGI_HOME": "odhome",
	} {
		p := filepath.Join(dir, sub)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(k, p)
	}
	for env, body := range map[string]string{
		"COPPICE_SPRIG_BIN":   fakeSprigScript,
		"COPPICE_DAISUGI_BIN": fakeDaisugiScript,
		"COPPICE_CLAUDE_BIN":  fakeClaudeScript,
	} {
		name := strings.ToLower(strings.TrimPrefix(env, "COPPICE_"))
		p := filepath.Join(dir, "fake-"+name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(env, p)
	}
	t.Setenv("FAKE_LOG", e.log)
	t.Setenv("FAKE_INFLIGHT", e.inflight)
	t.Setenv("FAKE_SENTINEL", "")
	t.Setenv("FAKE_MODE", "")
	t.Setenv("FAKE_MS", "")
	t.Setenv("FAKE_TREE_MS", "")
	t.Setenv("FAKE_TREE_GRANDCHILD", "")
	t.Setenv("FAKE_TREE_ERR_IF", "")
	t.Setenv("FAKE_BUNDLE_GRANDCHILD", "")
	t.Setenv("FAKE_BUNDLE_HANG", "")
	t.Setenv("FAKE_TREE_STATUS", "")
	t.Setenv("FAKE_TREE_EXIT", "")
	t.Setenv("FAKE_TREE_ERR", "")
	t.Setenv("FAKE_FAIL_URL", "")
	t.Setenv("FAKE_FAIL_ERR", "")
	t.Setenv("FAKE_DESK_WIRE", "")
	t.Setenv("FAKE_DESK_BASE", "")
	t.Setenv("FAKE_FREE_BASE", "")
	e.requireScratch(t)
	return e
}

// requireScratch fails the test when a binary the server resolves is not
// inside the test's scratch dir.
func (e *fakeEnv) requireScratch(t *testing.T) {
	t.Helper()
	for name, bin := range map[string]string{"claude": claudeBin(), "sprig": sprigBin(), "daisugi": daisugiBin()} {
		if !filepath.IsAbs(bin) || !strings.HasPrefix(bin, e.dir+string(filepath.Separator)) {
			t.Fatalf("the %s binary resolves to %q, outside the test's scratch %s", name, bin, e.dir)
		}
	}
	for _, k := range []string{"HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "OPENDAISUGI_HOME"} {
		if v := os.Getenv(k); !strings.HasPrefix(v, e.dir+string(filepath.Separator)) {
			t.Fatalf("%s is %q, outside the test's scratch %s", k, v, e.dir)
		}
	}
}

// read is a file's text, or "".
func read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// maxInflight is the most calls that ran at once, from the start/end lines.
func (e *fakeEnv) maxInflight() int {
	cur, max := 0, 0
	for _, l := range strings.Split(read(e.inflight), "\n") {
		switch {
		case strings.HasPrefix(l, "start "):
			cur++
			if cur > max {
				max = cur
			}
		case strings.HasPrefix(l, "end "):
			cur--
		}
	}
	return max
}

// waitFor polls cond until it holds or the time is up.
func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// noteTexts are the texts of the notes the server holds.
func noteTexts(s *Server) []string {
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	var out []string
	for _, n := range s.notes {
		out = append(out, n.Text)
	}
	return out
}

// grepTree lists every file under roots whose bytes hold needle.
func grepTree(roots []string, needle string) []string {
	var hits []string
	for _, root := range roots {
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), needle) {
				hits = append(hits, p)
			}
			return nil
		})
	}
	return hits
}

// fakeClaudeScript stands in for claude -p in the turn tests. It logs its
// argv (the system prompt as a size), saves it and its stdin, starts the MCP server
// its --mcp-config names, runs the calls in FAKE_TURN_SCRIPT through it,
// and prints a json result with usage and a thinking block. FAKE_TURN_MODE:
// ok (default), exit1, hang (sleeps 60 s), builtin (also runs the hook for
// the Bash tool and for a foreman tool, and logs the exit codes).
const fakeClaudeScript = `#!/usr/bin/python3
import json, os, subprocess, sys, time
argv = sys.argv[1:]
shown = list(argv)
system = b""
if "--system-prompt-file" in shown:
    i = shown.index("--system-prompt-file")
    system = open(shown[i + 1], "rb").read()
    shown[i + 1] = "<system file %d bytes>" % len(system)
sd = os.environ.get("FAKE_SYSTEM_DIR")
if sd:
    open(os.path.join(sd, "system-%d.txt" % (len(os.listdir(sd)) + 1)), "wb").write(system)
log = open(os.environ["FAKE_LOG"], "a")
def say(*a):
    print(*a, file=log, flush=True)
say("claude argv:", json.dumps(shown))
stdin = sys.stdin.buffer.read()
d = os.environ.get("FAKE_STDIN_DIR")
if d:
    n = len(os.listdir(d)) + 1
    open(os.path.join(d, "stdin-%d.txt" % n), "wb").write(stdin)
open(os.environ.get("FAKE_PIDFILE", os.devnull), "a").write("%d\n" % os.getpid())
mode = os.environ.get("FAKE_TURN_MODE", "ok")
if mode == "exit1":
    sys.stderr.write("claude: not logged in\n")
    sys.exit(1)
if mode == "hang":
    time.sleep(60)
if os.environ.get("FAKE_MS"):
    time.sleep(int(os.environ["FAKE_MS"]) / 1000.0)
cfg = json.loads(argv[argv.index("--mcp-config") + 1])["mcpServers"]["foreman"]
if mode == "builtin":
    settings = json.loads(argv[argv.index("--settings") + 1])
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    for tool in ("Bash", "mcp__foreman__zoom", "SomeNewBuiltIn"):
        r = subprocess.run(cmd, shell=True, input=json.dumps({"tool_name": tool}).encode(), capture_output=True)
        say("hook %s: exit %d" % (tool, r.returncode))
reply = os.environ.get("FAKE_TURN_REPLY") or "the foreman says hi"
calls = json.loads(os.environ.get("FAKE_TURN_SCRIPT") or "[]")
srv = subprocess.Popen([cfg["command"]] + cfg["args"], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
def rpc(i, method, params=None):
    msg = {"jsonrpc": "2.0", "id": i, "method": method}
    if params is not None:
        msg["params"] = params
    srv.stdin.write((json.dumps(msg) + "\n").encode())
    srv.stdin.flush()
    return json.loads(srv.stdout.readline())
rpc(1, "initialize", {})
tools = rpc(2, "tools/list")["result"]["tools"]
say("claude tools:", ",".join(t["name"] for t in tools))
for k, c in enumerate(calls):
    r = rpc(10 + k, "tools/call", {"name": c["name"], "arguments": c.get("args", {})})["result"]
    say("claude call %s: error=%s %s" % (c["name"], r["isError"], json.dumps(r["content"][0]["text"])[:200]))
srv.stdin.close()
srv.wait()
print(json.dumps({"type": "result", "subtype": "success", "is_error": False, "result": reply,
    "messages": [{"type": "thinking", "thinking": "SECRET-THOUGHT-123"}],
    "usage": {"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 70}}))
`
