package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"daisugi-verify/internal/install"
)

// unitHook is the install.UnitHook of one run. Install starts the pando
// follow service, and uninstall stops it. The real systemctl runs only for
// the real home of the user who runs it (never for a scratch HOME, so a test
// or a compare run cannot touch the owner's services) and only with
// DAISUGI_NO_SYSTEMCTL unset. A failure prints one line and goes on: the
// files are written either way.
func (e *Env) unitHook() func(action, unit string) error {
	if e.unitHookFn != nil {
		return e.unitHookFn
	}
	if e.env["DAISUGI_NO_SYSTEMCTL"] != "" {
		return nil
	}
	if h, err := passwdHome(); err != nil || h == "" || filepath.Clean(h) != filepath.Clean(e.home) {
		return nil
	}
	bin, err := install.LookPath(e.env)("systemctl")
	if err != nil {
		return nil
	}
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"--user"}, args...)...)
		cmd.Env = e.Environ
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", args[0], err, firstLineOf(string(out)))
		}
		return nil
	}
	return func(action, unit string) error {
		var err error
		switch action {
		case "enable":
			if err = run("daemon-reload"); err == nil {
				err = run("enable", "--now", unit)
			}
			if err == nil {
				e.out("Pando follow service is enabled and running (%s).\n", unit)
			}
		case "disable":
			err = run("disable", "--now", unit)
		case "reload":
			err = run("daemon-reload")
		}
		if err != nil {
			e.errf("Pando follow service: %v. Do it yourself: systemctl --user %s %s\n", err, unitAction(action), unit)
		}
		return err
	}
}

// passwdHome is the home the system's user database gives this user. It is
// not user.Current(): that one falls back to $HOME when the lookup fails (an
// NSS or LDAP user, a container with no passwd entry), and a scratch HOME
// would then pass for the real one. LookupId has no such fallback, so a
// failed lookup means no systemctl.
var passwdHome = func() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func unitAction(action string) string {
	switch action {
	case "enable":
		return "enable --now"
	case "disable":
		return "disable --now"
	}
	return "daemon-reload"
}

func firstLineOf(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
