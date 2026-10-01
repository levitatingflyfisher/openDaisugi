package sprig

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// KeyNames are the environment variables that can hold a provider key.
// No process sprig starts (a bash call, the claude backend, the gate)
// gets them, so a key in the operator's shell never reaches a tool, a
// session file or an answer.
var KeyNames = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "SPRIG_API_KEY"}

var (
	scrubMu    sync.Mutex
	scrubExtra []string
)

// SetScrubEnv adds names (from --scrub-env) to the variables no child
// process gets. Empty names are skipped.
func SetScrubEnv(names []string) {
	scrubMu.Lock()
	defer scrubMu.Unlock()
	scrubExtra = nil
	for _, n := range names {
		if n != "" {
			scrubExtra = append(scrubExtra, n)
		}
	}
}

func scrubbed(name string) bool {
	for _, n := range KeyNames {
		if n == name {
			return true
		}
	}
	scrubMu.Lock()
	defer scrubMu.Unlock()
	for _, n := range scrubExtra {
		if n == name {
			return true
		}
	}
	return false
}

// ChildEnv is the environment of every process sprig starts: its own
// less the key variables and the --scrub-env names, sorted by name. With
// dir set, PWD is that directory, as Go sets it for a command that runs
// there with the parent's environment.
func ChildEnv(dir string) []string {
	seen := map[string]int{}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if scrubbed(name) || (dir != "" && name == "PWD") {
			continue
		}
		if i, ok := seen[name]; ok {
			env[i] = kv // the last one wins, as exec does
			continue
		}
		seen[name] = len(env)
		env = append(env, kv)
	}
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			env = append(env, "PWD="+abs)
		}
	}
	sort.SliceStable(env, func(i, j int) bool {
		a, _, _ := strings.Cut(env[i], "=")
		b, _, _ := strings.Cut(env[j], "=")
		return a < b
	})
	return env
}

// ReadKeyFd reads a key from file descriptor fd to its end, drops one
// trailing newline and closes the descriptor. The error is a line for
// stderr, without the "sprig: " prefix.
func ReadKeyFd(flag string, fd int) (string, error) {
	if fd >= 0 && fd <= 2 {
		return "", fmt.Errorf("%s %d is stdin, stdout or stderr; pass a descriptor of 3 or more", flag, fd)
	}
	f := os.NewFile(uintptr(fd), flag)
	if f == nil {
		return "", fmt.Errorf("%s %d cannot be read", flag, fd)
	}
	b, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("%s %d cannot be read", flag, fd)
	}
	key := strings.TrimSuffix(string(b), "\n")
	return key, nil
}

// KeyHasControl reports whether a key holds a control byte (below 0x20,
// or DEL): such a key cannot go in a header, so sprig refuses it at once.
func KeyHasControl(key string) bool {
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] == 0x7f {
			return true
		}
	}
	return false
}

// CloseInheritedFds marks every descriptor above 2 close-on-exec, so no
// process sprig starts inherits one. Go opens its own descriptors that
// way; this covers the ones sprig's parent left open.
func CloseInheritedFds() {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= 2 {
			continue
		}
		syscall.CloseOnExec(fd)
	}
}

// Redact replaces the key in text with [key]. An empty key changes
// nothing.
func Redact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[key]")
}

// redactingModel wraps a model so that the key never reaches an error
// it returns: a provider can echo the request in its error body.
type redactingModel struct {
	Model
	key string
}

func (r redactingModel) Next(history []Message) (Message, error) {
	msg, err := r.Model.Next(history)
	if err != nil {
		if s := Redact(err.Error(), r.key); s != err.Error() {
			err = fmt.Errorf("%s", s)
		}
	}
	return msg, err
}

func (r redactingModel) ModelName() string {
	if n, ok := r.Model.(interface{ ModelName() string }); ok {
		return n.ModelName()
	}
	return ""
}
