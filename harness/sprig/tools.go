package sprig

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// str coerces a tool-input value to a string (inputs arrive as map[string]any).
func str(v any) string { s, _ := v.(string); return s }

// The four tools — pi's floor. A frontier model already knows how to be a coding
// agent with read/write/edit/bash; everything past this must earn its tokens.
// Every one runs ONLY after the Executor's gate allows the call.

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
// dangerous one — which is exactly why the gate exists. input: {"cmd": string}.
type BashTool struct{}

func (BashTool) Name() string { return "bash" }
func (BashTool) Run(in map[string]any) (string, error) {
	out, err := exec.Command("sh", "-c", str(in["cmd"])).CombinedOutput()
	return string(out), err
}

// DefaultTools is the whole tool surface — exactly four, keyed by name.
func DefaultTools() map[string]Tool {
	return map[string]Tool{
		"read":  ReadTool{},
		"write": WriteTool{},
		"edit":  EditTool{},
		"bash":  BashTool{},
	}
}
