package gate

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pyjson"
)

var herdrPaneID = lazyre.New(`^[A-Za-z0-9._:-]{1,128}$`)

var herdrState = map[string]string{"working": "working", "blocked": "blocked", "idle": "idle", "done": "idle"}

// sendHerdrReport is _state_report._send_herdr_report: the herdr CLI,
// started with the gate's environment and killed at the deadline.
func (r *runner) sendHerdrReport(pane string, ev *pyjson.Object, deadline time.Time) {
	if strings.HasPrefix(pane, "-") || !herdrPaneID().MatchString(pane) {
		return
	}
	state, _ := ev.Value("state").(string)
	mapped, ok := herdrState[state]
	if !ok {
		return
	}
	bin := r.which("herdr")
	if bin == "" {
		return
	}
	cmd := exec.Command(bin, "pane", "report-agent", "--source", "daisugi", "--agent", pyStrOr(ev.Value("harness")),
		"--state", mapped, "--", pane)
	cmd.Env = r.environ()
	cmd.Stdin = os.Stdin
	if cmd.Start() != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	remaining := time.Until(deadline)
	if remaining < time.Millisecond {
		remaining = time.Millisecond
	}
	select {
	case <-done:
	case <-time.After(remaining):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// which is shutil.which(name, path=PATH of the gate's environment).
func (r *runner) which(name string) string {
	path, ok := r.env["PATH"]
	if !ok {
		// os.confstr("CS_PATH") on glibc.
		path = "/bin:/usr/bin"
	}
	if path == "" {
		return ""
	}
	seen := map[string]bool{}
	for _, dir := range strings.Split(path, ":") {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		// os.path.join(dir, name).
		p := dir + "/" + name
		switch {
		case dir == "":
			p = name
		case strings.HasSuffix(dir, "/"):
			p = dir + name
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if syscall.Access(p, 1) == nil {
			return p
		}
	}
	return ""
}
