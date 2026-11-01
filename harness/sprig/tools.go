package sprig

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// str coerces a tool-input value to a string (inputs arrive as map[string]any).
func str(v any) string { s, _ := v.(string); return s }

// The four tools — pi's floor. A frontier model already knows how to be a coding
// agent with read/write/edit/bash; everything past this must earn its tokens.
// Every one runs ONLY after the Executor's warden allows the call.

// ReadTool returns the contents of a file. input: {"path": string}.
type ReadTool struct{}

func (ReadTool) Name() string { return "read" }
func (ReadTool) Run(in map[string]any) (string, error) {
	b, err := os.ReadFile(str(in["path"]))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteTool writes content to a file, creating parent directories.
// input: {"path": string, "content": string}.
type WriteTool struct{}

func (WriteTool) Name() string { return "write" }
func (WriteTool) Run(in map[string]any) (string, error) {
	path := str(in["path"])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(str(in["content"])), 0o644); err != nil {
		return "", err
	}
	return "wrote " + path, nil
}

// EditTool replaces a UNIQUE occurrence of old with new, refusing ambiguous or
// missing matches — the same safety the real editor enforces so an edit can
// never land in the wrong place. input: {"path","old","new"}.
type EditTool struct{}

func (EditTool) Name() string { return "edit" }
func (EditTool) Run(in map[string]any) (string, error) {
	path, old, nw := str(in["path"]), str(in["old"]), str(in["new"])
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	body := string(b)
	switch strings.Count(body, old) {
	case 0:
		return "", fmt.Errorf("edit: %q not found in %s", old, path)
	case 1:
		// ok
	default:
		return "", fmt.Errorf("edit: %q is not unique in %s (matches %d times)", old, path, strings.Count(body, old))
	}
	if err := os.WriteFile(path, []byte(strings.Replace(body, old, nw, 1)), 0o644); err != nil {
		return "", err
	}
	return "edited " + path, nil
}

// BashTool runs a shell command and returns its combined output. The universal
// escape hatch (a CLI script + README beats a 13-18k-token MCP). This is the
// dangerous one — which is exactly why the warden exists. input: {"cmd": string}.
// StepCap, when set, is the longest one call may run: past it the call's
// whole group is killed and the call fails with a line that names it. The
// run as a whole has no cap here; its starter sets one if it wants one.
type BashTool struct{ StepCap time.Duration }

func (BashTool) Name() string { return "bash" }
func (b BashTool) Run(in map[string]any) (string, error) {
	text := str(in["cmd"])
	cmd := exec.Command("sh", "-c", text)
	cmd.Env = ChildEnv("")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	killed, err := runInGroupFor(cmd, b.StepCap)
	if killed {
		return out.String(), fmt.Errorf("the step %q ran past the step cap of %s and was killed", oneLineCmd(text), b.StepCap)
	}
	return out.String(), err
}

// oneLineCmd is a command as a short line for a message.
func oneLineCmd(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "..."
	}
	return s
}

// StepCapEnv names the step cap in seconds when --step-timeout is not
// given: daisugi tree run sets it from the envelope's max_execution_time_s.
const StepCapEnv = "SPRIG_STEP_TIMEOUT_S"

// DefaultTools is the whole tool surface — exactly four, keyed by name.
func DefaultTools() map[string]Tool {
	return map[string]Tool{
		"read":  ReadTool{},
		"write": WriteTool{},
		"edit":  EditTool{},
		"bash":  BashTool{},
	}
}

// AllToolNames is the four tools in sprig's own order: the order a prompt
// lists them in, and the default of --tools.
var AllToolNames = []string{"read", "write", "edit", "bash"}

// WallToolNames is every name --tools takes, in sprig's order: the four
// tools, spawn (which needs --tree-cmd) and help.
var WallToolNames = []string{"read", "write", "edit", "bash", "spawn", "help"}

// ParseToolWall reads a --tools value: a comma list of tool names. It
// returns the named tools once each, in sprig's order. An empty value or
// a name that is not a tool is an error, worded for one line on stderr.
func ParseToolWall(list string) ([]string, error) {
	if list == "" {
		return nil, fmt.Errorf("--tools is empty; name one or more of read, write, edit, bash, spawn, help")
	}
	want := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		if !slices.Contains(WallToolNames, name) {
			return nil, fmt.Errorf("--tools: unknown tool %q; the tools are read, write, edit, bash, spawn, help", name)
		}
		want[name] = true
	}
	var wall []string
	for _, name := range WallToolNames {
		if want[name] {
			wall = append(wall, name)
		}
	}
	return wall, nil
}

// ToolsFor is the tool map for a wall: only the named tools. A call to any
// other tool is then refused as an unknown tool.
func ToolsFor(wall []string) map[string]Tool { return ToolsWithStepCap(wall, 0) }

// ToolsWithStepCap is ToolsFor with a step cap on bash (0 is none).
func ToolsWithStepCap(wall []string, stepCap time.Duration) map[string]Tool {
	all := DefaultTools()
	all["bash"] = BashTool{StepCap: stepCap}
	tools := map[string]Tool{}
	for _, name := range wall {
		if t, ok := all[name]; ok { // a name that is no tool is left out
			tools[name] = t
		}
	}
	return tools
}
