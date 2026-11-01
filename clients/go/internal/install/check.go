package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/wardenroot"
)

// RestartLine is install.RESTART_LINE: said after an install writes.
const RestartLine = "Restart your agent so the hook loads. To check it works, use one tool, " +
	"then run: daisugi hook list"

// walkCommands is install._walk_commands: the program of every hook command
// in a settings tree.
func walkCommands(node any, out *[]string) {
	switch n := node.(type) {
	case *pyjson.Object:
		if cmd, ok := n.Value("command").(string); ok {
			*out = append(*out, config.HookCommandPrograms(cmd)...)
		}
		for _, k := range n.Keys() {
			walkCommands(n.Value(k), out)
		}
	case []any:
		for _, v := range n {
			walkCommands(v, out)
		}
	}
}

// mcpCommand is install._mcp_command: the command text at a key path.
func mcpCommand(node any, path ...string) []string {
	for _, k := range path {
		o, ok := node.(*pyjson.Object)
		if !ok {
			return nil
		}
		node = o.Value(k)
	}
	if o, ok := node.(*pyjson.Object); ok {
		if cmd, ok := o.Value("command").(string); ok && cmd != "" {
			return []string{cmd}
		}
	}
	return nil
}

// readJSONFile reads a JSON (or, with json5, a JSON5) file; nil when it is
// not there or does not read.
func readJSONFile(p string, json5 bool) any {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	text := string(raw)
	if json5 {
		text = stripJSON5(text)
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	return v
}

// WrittenPrograms is install.written_programs: the program word of each
// hook and MCP command a runtime's config holds on disk, in file order,
// without repeats. It reads the files, not what the install meant to write.
func WrittenPrograms(rt Runtime, home string) []string {
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	var words []string
	switch rt.Key {
	case "claude":
		walkCommands(readJSONFile(j(".claude/settings.json"), false), &words)
		words = append(words, mcpCommand(readJSONFile(j(".claude.json"), false), "mcpServers", "opendaisugi")...)
	case "codex":
		if raw, err := os.ReadFile(j(".codex/config.toml")); err == nil {
			if m := codexMCPRe.FindStringSubmatch("\n" + string(raw)); m != nil {
				if v, err := pyjson.Loads(`"` + m[1] + `"`); err == nil {
					if s, ok := v.(string); ok {
						words = append(words, s)
					}
				}
			}
		}
		walkCommands(readJSONFile(j(".codex/hooks.json"), false), &words)
	case "hermes":
		cfg, _, _, err := yamlOf(j(".hermes/config.yaml"))
		if err == nil && cfg != nil {
			words = append(words, mcpCommand(cfg, "mcp_servers", "opendaisugi")...)
			walkCommands(cfg.Value("hooks"), &words)
		}
	case "openclaw":
		words = append(words, mcpCommand(readJSONFile(j(".openclaw/openclaw.json"), true), "mcp", "servers", "opendaisugi")...)
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// whyUnrunnable is install._why_unrunnable: why the agent cannot start a
// program word, or "" when it can.
func whyUnrunnable(word string) string {
	if strings.Contains(word, "/") {
		if st, err := os.Stat(word); err != nil || !st.Mode().IsRegular() {
			return "is not there"
		}
		if syscall.Access(word, 1) != nil {
			return "is not executable"
		}
		return ""
	}
	if _, err := exec.LookPath(word); err != nil {
		return "is not on the PATH"
	}
	return ""
}

// UnrunnableCommands is install.unrunnable_commands: one plain line for each
// runtime whose hook or MCP command, read back from the config on disk,
// names a program the agent cannot start. Empty when every one can run.
func UnrunnableCommands(runtimes []Runtime, home string) []string {
	var out []string
	for _, rt := range runtimes {
		var bad []string
		for _, w := range WrittenPrograms(rt, home) {
			if why := whyUnrunnable(w); why != "" {
				bad = append(bad, "The command "+w+" "+why)
			}
		}
		if len(bad) > 0 {
			out = append(out, "["+rt.Name+"] "+strings.Join(bad, ", and ")+
				", so the agent cannot start its hook or MCP server. Install daisugi there, then run: daisugi install")
		}
	}
	return out
}

// TemporaryExeNote is install.temporary_exe_note: one line when the entry
// point lives in uv's cache, where a cache clean removes it. "" otherwise.
func TemporaryExeNote(self string) string {
	paths := []string{self}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		paths = append(paths, real)
	}
	for _, p := range paths {
		if strings.Contains(p, "/.cache/uv/") {
			return "The path " + self + " is temporary: it is in uv's cache, and a cache clean " +
				"removes it. Install daisugi with: uv tool install opendaisugi, then run " +
				"daisugi install again."
		}
	}
	return ""
}
