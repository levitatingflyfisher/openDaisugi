package supervise

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// Decision is approval.ApprovalDecision.
type Decision struct {
	Approved   bool
	ApprovedBy string
	Reason     string
}

// Approver is approval.ApprovalStrategy. An error is an exception the
// strategy raised, worded "Type: message".
type Approver interface {
	Decide(step, env *pyjson.Object) (Decision, error)
}

// Always is CallbackStrategy(lambda step, env: True), the orchestrator's
// approval.
type Always struct{}

func (Always) Decide(_, _ *pyjson.Object) (Decision, error) {
	return Decision{true, "callback", "user-supplied callback approved"}, nil
}

// Default is approval.default_strategy(): the allowlist, then
// DAISUGI_APPROVE, then a prompt on a terminal, then deny.
type Default struct {
	Getenv func(string) (string, bool)
	Stdin  io.Reader
	Stdout io.Writer
	// Terminal reports whether stdin and stdout are both terminals.
	Terminal func() bool
}

var validApprove = []string{"always", "never", "interactive", "auto"}

func (d Default) Decide(step, env *pyjson.Object) (Decision, error) {
	if str(step, "type") == "shell" {
		cmd := pystr.Strip(str(step, "command"))
		if cmd != "" && !verify.HasShellMetachar(cmd) {
			first := pystr.Split(cmd)[0]
			perms, _ := env.Value("permissions").(*pyjson.Object)
			if perms != nil {
				for _, a := range asList(perms.Value("shell_allowlist")) {
					if a == first {
						return Decision{true, "allowlist", fmt.Sprintf("'%s' is in shell_allowlist", first)}, nil
					}
				}
			}
		}
	}
	v, _ := d.Getenv("DAISUGI_APPROVE")
	value := pystr.Lower(pystr.Strip(v))
	if value != "" {
		ok := false
		for _, x := range validApprove {
			ok = ok || x == value
		}
		if !ok {
			sorted := append([]string{}, validApprove...)
			sort.Strings(sorted)
			return Decision{}, fmt.Errorf("ValueError: DAISUGI_APPROVE=%s is not a valid value; expected one of %s",
				pystr.Repr(value), pystr.ReprList(sorted))
		}
		switch value {
		case "always":
			return Decision{true, "env", "DAISUGI_APPROVE=always"}, nil
		case "never":
			return Decision{false, "env", "DAISUGI_APPROVE=never"}, nil
		}
	}
	if d.Terminal == nil || !d.Terminal() {
		return Decision{false, "denied", "no TTY available; set DAISUGI_APPROVE=always for non-interactive approval"}, nil
	}
	what := str(step, "command")
	if what == "" {
		what = str(step, "path")
	}
	if what == "" {
		what = str(step, "url")
	}
	if what == "" {
		what = str(step, "id")
	}
	fmt.Fprintf(d.Stdout, "Approve step %s (%s)? [y/N] ", pystr.Repr(str(step, "id")), what)
	line, err := bufio.NewReader(d.Stdin).ReadString('\n')
	if err != nil && line == "" {
		// input() at end of file raises EOFError.
		return Decision{}, errors.New("EOFError: EOF when reading a line")
	}
	answer := pystr.Lower(pystr.Strip(strings.TrimSuffix(line, "\n")))
	ok := answer == "y" || answer == "yes"
	return Decision{ok, "tty", "user answered " + pystr.Repr(answer)}, nil
}

// IsTerminal reports whether f is a terminal (isatty).
func IsTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
