package install

// The one-time move after the rename (gate -> warden, gateway -> router). // names-ok
// install.move_old_state in the Python oracle: the owner's state still
// carries the old names; `daisugi install` moves each once and says one
// line per move. Nothing is deleted. The old names below are read on
// purpose; each such line carries a names-ok tag for the names check.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
)

// MoveRefused is a move that stops the run: exit 2, one line said.
type MoveRefused struct{ Line string }

func (e *MoveRefused) Error() string { return e.Line }

// renamedConfigKeys are the six config keys the rename moved.
var renamedConfigKeys = config.RenamedKeys

var oldHookRewrites = []struct {
	re  *regexp.Regexp
	rep string
}{
	{regexp.MustCompile(`\bDAISUGI_GATE_`), "DAISUGI_WARDEN_"},                     // names-ok: old env prefix
	{regexp.MustCompile(`\bOPENDAISUGI_GATE_`), "OPENDAISUGI_WARDEN_"},             // names-ok: old env prefix
	{regexp.MustCompile(`opendaisugi\.gate_client`), "opendaisugi.warden_client"},  // names-ok: old module
	{regexp.MustCompile(`opendaisugi\.gate([^A-Za-z]|$)`), "opendaisugi.warden$1"}, // names-ok: old module
	{regexp.MustCompile(`(daisugi) gate (check)`), "$1 warden $2"},                 // names-ok: old verb
	{regexp.MustCompile(`(\s)gate (check)`), "${1}warden $2"},                      // names-ok: old verb
	{regexp.MustCompile(`--gate-root`), "--warden-root"},                           // names-ok: old flag
	{regexp.MustCompile(`\bgate\.sock\b`), "warden.sock"},                          // names-ok: old socket name
}

// newHookCommand is _new_hook_command: an old-form warden hook in today's
// words, or false when it is not one of ours. The mode stays.
func newHookCommand(command, dataDir string) (string, bool) {
	n := command
	for _, r := range oldHookRewrites {
		n = r.re.ReplaceAllString(n, r.rep)
	}
	// The hook reads the warden dir by path and install moves that dir, so
	// the path moves with it (plain or quoted).
	rootRe := regexp.MustCompile(`(--root\s+)(['"]?)` + regexp.QuoteMeta(filepath.Join(dataDir, "gate")) + `(['"]?)(\s|$)`) // names-ok: old dir
	n = rootRe.ReplaceAllStringFunc(n, func(m string) string {
		g := rootRe.FindStringSubmatch(m)
		if g[2] != g[3] {
			return m
		}
		return g[1] + g[2] + filepath.Join(dataDir, "warden") + g[3] + g[4]
	})
	if n == command {
		return "", false
	}
	if _, ok := config.WardenHookArgs(n); !ok {
		return "", false
	}
	return n, true
}

// rewriteOldHooks is _rewrite_old_hooks: each old-form warden hook command
// in the file, in place. True when a command changed.
func rewriteOldHooks(path, dataDir string, dryRun bool) bool {
	v, st, err := readJSON(path)
	if err != nil || st != readOK {
		return false
	}
	s, isObj := v.(*pyjson.Object)
	if !isObj {
		return false
	}
	hv, _ := s.Get("hooks")
	hooks, isObj := hv.(*pyjson.Object)
	if !isObj {
		return false
	}
	pv, _ := hooks.Get("PreToolUse")
	pre, isList := pv.([]any)
	if !isList {
		return false
	}
	changed := false
	for _, e := range pre {
		entry, isObj := e.(*pyjson.Object)
		if !isObj {
			continue
		}
		iv, _ := entry.Get("hooks")
		inner, isList := iv.([]any)
		if !isList {
			continue
		}
		for _, h := range inner {
			hook, isObj := h.(*pyjson.Object)
			if !isObj {
				continue
			}
			cv, _ := hook.Get("command")
			cmd, isStr := cv.(string)
			if !isStr {
				continue
			}
			if n, ok := newHookCommand(cmd, dataDir); ok {
				hook.Set("command", n)
				changed = true
			}
		}
	}
	if changed && !dryRun {
		if err := Backup(path); err == nil {
			_ = os.WriteFile(path, []byte(dumpSettings(s)), 0o644)
		}
	}
	return changed
}

func socketListens(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// MoveOldState is install.move_old_state. The lines it says go to out and
// into the result. A refusal returns a *MoveRefused after its line.
func MoveOldState(dataDir, home string, env map[string]string, now time.Time, out io.Writer, dryRun bool) ([]string, error) {
	var lines []string
	say := func(format string, a ...any) {
		l := fmt.Sprintf(format, a...)
		lines = append(lines, l)
		fmt.Fprintln(out, l)
	}
	refuse := func(format string, a ...any) ([]string, error) {
		say(format, a...)
		return lines, &MoveRefused{Line: lines[len(lines)-1]}
	}
	verb := "moved"
	if dryRun {
		verb = "would move"
	}

	oldW, newW := filepath.Join(dataDir, "gate"), filepath.Join(dataDir, "warden") // names-ok: old dir
	if isDirNoFollow(oldW) {
		if _, err := os.Lstat(newW); err == nil {
			return refuse("both %s and %s exist; move one aside by hand, then run install again", oldW, newW)
		}
		sock := filepath.Join(oldW, "gate.sock") // names-ok: old socket
		if socketListens(sock) {
			return refuse("a gate server still listens on %s; stop it (the pid is in %s) and run install again", // names-ok: old words
				sock, filepath.Join(oldW, "gate.lock")) // names-ok: old lock
		}
		say("%s %s to %s", verb, oldW, newW)
		if !dryRun {
			if err := os.Rename(oldW, newW); err != nil {
				return lines, err
			}
		}
	}

	oldR, newR := filepath.Join(dataDir, "gateway"), filepath.Join(dataDir, "router") // names-ok: old dir
	if isDirNoFollow(oldR) {
		if _, err := os.Lstat(newR); err != nil {
			say("%s %s to %s", verb, oldR, newR)
			if !dryRun {
				if err := os.Rename(oldR, newR); err != nil {
					return lines, err
				}
			}
		} else {
			// router/ already holds the delegation and label journals. The
			// old files join them, one by one; a name in both stops the move.
			ents, _ := os.ReadDir(oldR)
			var clash []string
			for _, ent := range ents {
				if _, err := os.Lstat(filepath.Join(newR, ent.Name())); err == nil {
					clash = append(clash, ent.Name())
				}
			}
			if len(clash) > 0 {
				sort.Strings(clash)
				return refuse("both %s and %s hold %s; move one aside by hand, then run install again",
					oldR, newR, strings.Join(clash, ", "))
			}
			say("%s %s into %s", verb, oldR, newR)
			if !dryRun {
				for _, ent := range ents {
					if err := os.Rename(filepath.Join(oldR, ent.Name()), filepath.Join(newR, ent.Name())); err != nil {
						return lines, err
					}
				}
				_ = os.Remove(oldR)
			}
		}
	}

	cfg := filepath.Join(dataDir, "config.yaml")
	if raw, err := os.ReadFile(cfg); err == nil {
		text := string(raw)
		n := text
		for _, k := range renamedConfigKeys {
			re := regexp.MustCompile(`(?m)^(\s*)` + regexp.QuoteMeta(k[0]) + `(\s*:)`)
			if re.MatchString(n) {
				n = re.ReplaceAllString(n, "${1}"+k[1]+"${2}")
				say("config: %s is now %s", k[0], k[1])
			}
		}
		if n != text && !dryRun {
			if err := os.WriteFile(cfg, []byte(n), 0o644); err != nil {
				return lines, err
			}
		}
	}

	for _, f := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".codex", "hooks.json")} {
		if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() && rewriteOldHooks(f, dataDir, dryRun) {
			say("replaced the old warden hook in %s", f)
		}
	}

	aside := filepath.Join(dataDir, "moved-aside", now.UTC().Format("20060102T150405Z"))
	piOld := filepath.Join(filepath.Dir(PiDir(home)), "daisugi-gate") // names-ok: old extension dir
	ocNew := OpenCodePath(home, env)
	ocOld := filepath.Join(filepath.Dir(ocNew), "daisugi-gate.ts")                                   // names-ok: old plugin file
	for _, m := range [][2]string{{piOld, "pi-daisugi-gate"}, {ocOld, "opencode-daisugi-gate.ts"}} { // names-ok: old names
		if _, err := os.Lstat(m[0]); err != nil {
			continue
		}
		dest := filepath.Join(aside, m[1])
		say("%s aside %s to %s", verb, m[0], dest)
		if !dryRun {
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return lines, err
			}
			if err := os.Rename(m[0], dest); err != nil {
				return lines, err
			}
		}
	}

	// The delegate tool is now spawn. An envelope that allows the old tool
	// name is the owner's to edit; install edits no envelope.
	root := newW
	if _, err := os.Lstat(newW); err != nil {
		root = oldW
	}
	envs, _ := filepath.Glob(filepath.Join(root, "envelopes", "*.json"))
	sort.Strings(envs)
	for _, f := range envs {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc struct {
			Permissions struct {
				MCPAllowlist []string `json:"mcp_allowlist"`
			} `json:"permissions"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		hasOld, hasNew := false, false
		for _, a := range doc.Permissions.MCPAllowlist {
			hasOld = hasOld || a == "opendaisugi/delegate" // names-ok: old tool
			hasNew = hasNew || a == "opendaisugi/spawn"
		}
		if hasOld && !hasNew {
			say("envelope %s allows mcp opendaisugi/delegate; add opendaisugi/spawn (the tool was renamed)", // names-ok: old tool
				strings.TrimSuffix(filepath.Base(f), ".json"))
		}
	}
	return lines, nil
}
