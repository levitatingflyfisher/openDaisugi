package install

import (
	"fmt"
	"os"
	"path/filepath"
)

// PortEnv names the port whose daisugi writes the hooks (ruling
// PK-R-15). A package installs the three ports beside each other, under
// the names of PortBinaries; `install` in any of them hands the whole
// command to the one PortEnv names, once, at install time. The hook it
// writes then runs that port's binary directly, so the gate's hot path
// has no extra exec.
const PortEnv = "DAISUGI_PORT"

// PortHopEnv is set on the one hand-over, so a binary that turns out to
// be another port stops instead of handing over again.
const PortHopEnv = "DAISUGI_PORT_HOP"

// PortBinaries is the file name each port's daisugi has beside the
// others: the Go one is the plain daisugi.
var PortBinaries = map[string]string{"go": "daisugi", "rust": "daisugi-rs", "python": "daisugi-py"}

// PortHop is where `install` runs: "" for this binary (own is its port,
// self its path as Self gives it), or the path of the sibling binary
// PortEnv names. The error is one line, said with exit 2; nothing is
// changed.
func PortHop(own, self string, env map[string]string) (string, error) {
	want := env[PortEnv]
	if want == "" || want == own {
		return "", nil
	}
	name, ok := PortBinaries[want]
	if !ok {
		return "", fmt.Errorf("%s must be go, rust or python, not %s. Nothing was changed.", PortEnv, want)
	}
	if env[PortHopEnv] != "" {
		return "", fmt.Errorf("%s is %s, but the daisugi it ran is the %s port. Nothing was changed.", PortEnv, want, own)
	}
	target := filepath.Join(filepath.Dir(self), name)
	if st, err := os.Stat(target); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s is %s, but there is no %s beside %s. Install it, or unset %s. Nothing was changed.",
			PortEnv, want, name, self, PortEnv)
	}
	return target, nil
}
