package install

import (
	"strings"

	"daisugi-verify/internal/config"
)

// NotIsolatedNote is install._NOT_ISOLATED_NOTE: added to the "different
// mode/config" warning when a gate hook there runs the Python module
// without -I (SX-R-9).
const NotIsolatedNote = " That hook also runs Python without -I, so it can import a module from the " +
	"agent's working directory: run `daisugi install --uninstall` and then " +
	"`daisugi install --gate` to replace it."

// notIsolatedNote is install._not_isolated_note: a plain text test, as the
// oracle's.
func notIsolatedNote(existing []any) string {
	for _, c := range existing {
		s, ok := c.(string)
		if ok && config.GateHookKind(c) != config.KindNone &&
			(strings.Contains(s, "-m opendaisugi.gate") || strings.Contains(s, "-mopendaisugi.gate")) &&
			!strings.Contains(s, " -I -m ") {
			return NotIsolatedNote
		}
	}
	return ""
}
