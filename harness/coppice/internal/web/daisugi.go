package web

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The floor asks daisugi for cards and for the delegation tree. It runs
// the daisugi binary as a child process and never reads or writes the
// rankings or the tree ledger itself (W3K-5). COPPICE_DAISUGI_BIN names
// the binary, as the fence does. COPPICE_DAISUGI_DATA_DIR names the data
// directory, and daisugi's own default holds when it is empty.
const (
	daisugiTimeout = 10 * time.Second
	daisugiMaxOut  = 1 << 20
)

func daisugiBin() string {
	if b := os.Getenv("COPPICE_DAISUGI_BIN"); b != "" {
		return b
	}
	return "daisugi"
}

func daisugiDataDir() string { return os.Getenv("COPPICE_DAISUGI_DATA_DIR") }

// daisugiArgs adds --data-dir to args when the floor has one.
func daisugiArgs(args ...string) []string { return daisugiArgsIn(daisugiDataDir(), args...) }

// daisugiArgsIn adds --data-dir dir to args; an empty dir is daisugi's own
// default.
func daisugiArgsIn(dir string, args ...string) []string {
	if dir != "" {
		args = append(args, "--data-dir", dir)
	}
	return args
}

// dataSources are the data directories the floor reads cards from, in
// order: the configured one (empty for daisugi's default), then the extra
// ones the server owns, such as the steward's work dir, that exist and are
// not the first again. A card shows once, whichever dir holds it.
func (s *Server) dataSources() []string {
	first := daisugiDataDir()
	out := []string{first}
	for _, d := range s.opts.ExtraDataDirs {
		if d == "" || d == first {
			continue
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == d
		}
		if !dup {
			out = append(out, d)
		}
	}
	return out
}

// dataLabel names a source in a reply.
func dataLabel(dir string) string {
	if dir == "" {
		return "default"
	}
	return dir
}

// capBuf keeps the first daisugiMaxOut bytes and drops the rest.
type capBuf struct {
	b    bytes.Buffer
	over bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	room := daisugiMaxOut - c.b.Len()
	if room <= 0 {
		c.over = true
		return len(p), nil
	}
	if len(p) > room {
		c.b.Write(p[:room])
		c.over = true
		return len(p), nil
	}
	return c.b.Write(p)
}

// runDaisugi runs daisugi with args. The exit code is -1 when the child
// did not run or did not exit by itself.
func runDaisugi(ctx context.Context, args ...string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, daisugiTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, daisugiBin(), args...)
	var out, errb capBuf
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	stdout, stderr = out.b.String(), errb.b.String()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) && ee.ExitCode() >= 0 {
		return stdout, stderr, ee.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return stdout, stderr, -1, errors.New("timed out after 10 s")
	}
	return stdout, stderr, -1, runErr
}

// firstLine is the first non-empty line of s.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// idPattern is what a choice id, an attempt id or an ask id looks like.
// It never starts with a dash, so it cannot read as a flag.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,127}$`)

// newGit is a git command for dir. Tests do not replace it.
func newGit(ctx context.Context, dir string, args ...string) *exec.Cmd {
	// No file system monitor and no hooks directory: a config a branch child
	// wrote must not run a program of its own.
	guard := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"-c", "diff.ignoreSubmodules=all", "-c", "submodule.recurse=false", "-C", dir}
	return exec.CommandContext(ctx, "git", append(guard, args...)...)
}
