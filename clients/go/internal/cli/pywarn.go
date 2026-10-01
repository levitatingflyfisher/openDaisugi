package cli

import (
	"fmt"
	"regexp"
	"strings"

	"daisugi-verify/internal/pystr"
)

// builtinWarnings are the warning categories of Python's builtins, each
// with whether a UserWarning is an instance of it.
var builtinWarnings = map[string]bool{
	"Warning": true, "UserWarning": true, "DeprecationWarning": false,
	"PendingDeprecationWarning": false, "SyntaxWarning": false, "RuntimeWarning": false,
	"FutureWarning": false, "ImportWarning": false, "UnicodeWarning": false,
	"BytesWarning": false, "ResourceWarning": false, "EncodingWarning": false,
}

// userWarningShown is whether Python prints a UserWarning with text msg,
// raised from a module this binary cannot name, under the filters
// PYTHONWARNINGS sets (warnings._processoptions over the default
// filters, which show it). ok is false for a setting whose effect this
// binary does not model: an invalid option (Python prints a note at
// start), the "error" action (the warning becomes an exception), a
// filter that names a module or a line, or a category that is not one of
// the builtins.
func userWarningShown(pythonWarnings, msg string) (show, ok bool) {
	if pythonWarnings == "" {
		return true, true
	}
	type filter struct{ action, message string }
	var filters []filter // in the order of the option
	for _, opt := range strings.Split(pythonWarnings, ",") {
		parts := strings.Split(opt, ":")
		if len(parts) > 5 {
			return false, false
		}
		for len(parts) < 5 {
			parts = append(parts, "")
		}
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		action, message, category, module, lineno := parts[0], parts[1], parts[2], parts[3], parts[4]
		switch {
		case action == "":
			action = "default"
		case action == "all":
			action = "always"
		default:
			found := ""
			for _, a := range []string{"default", "always", "ignore", "module", "once", "error"} {
				if strings.HasPrefix(a, action) {
					found = a
					break
				}
			}
			if found == "" {
				return false, false
			}
			action = found
		}
		if module != "" || (lineno != "" && lineno != "0") {
			return false, false
		}
		if category == "" {
			category = "Warning"
		}
		category = strings.TrimPrefix(category, "builtins.")
		isUser, known := builtinWarnings[category]
		if !known {
			return false, false
		}
		// A filter that shows a category beyond UserWarning shows the
		// warnings Python's libraries raise too (DeprecationWarning and
		// the rest), which this binary cannot know.
		if category != "UserWarning" && action != "ignore" {
			return false, false
		}
		if isUser {
			filters = append(filters, filter{action, message})
		}
	}
	// Each option goes to the front of the filters, so the last one that
	// matches wins.
	for i := len(filters) - 1; i >= 0; i-- {
		f := filters[i]
		if f.message != "" && !regexp.MustCompile("(?i)^"+regexp.QuoteMeta(f.message)).MatchString(msg) {
			continue
		}
		if f.action == "error" {
			return false, false
		}
		return f.action != "ignore", true
	}
	return true, true
}

// staleWarner prints the stale-embeddings UserWarning once per process,
// as _warn_stale_embeddings_once does, in the form the compare reads:
// "UserWarning: <text>" (Python prefixes the file and line of a frame of
// its own and adds that frame's source line).
type staleWarner struct {
	e    *Env
	done bool
}

func (w *staleWarner) warn(msg string) {
	if w.done || msg == "" {
		return
	}
	w.done = true
	pw, _ := w.e.lookup("PYTHONWARNINGS")
	if show, _ := userWarningShown(pw, msg); show {
		w.e.errf("UserWarning: %s\n", msg)
	}
}

// staleOnce prints the stale-embeddings UserWarning for a command that
// finds and then prints nothing more of the warning, or the refusal's
// reason when PYTHONWARNINGS holds a filter this binary does not model.
func (e *Env) staleOnce(msg string) error {
	pw, _ := e.lookup("PYTHONWARNINGS")
	show, ok := userWarningShown(pw, msg)
	if !ok {
		return fmt.Errorf("the pathway store warns of stale embeddings under PYTHONWARNINGS=%s, a filter this binary does not read the oracle's way", pystr.Repr(pw))
	}
	if show {
		e.errf("UserWarning: %s\n", msg)
	}
	return nil
}
