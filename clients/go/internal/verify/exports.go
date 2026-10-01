package verify

// Exported forms of the permission-stage helpers, for the gate binary,
// which builds the oracle's violation messages around the same checks.

// HeadAllowed is verify._head_allowed.
func HeadAllowed(head string, allowlist []string) bool { return headAllowed(head, allowlist) }

// ExtractShellHead is verify._extract_shell_head.
func ExtractShellHead(stripped string) (string, bool) { return extractShellHead(stripped) }

// HasShellMetachar is verify._SHELL_METACHAR_RE.search.
func HasShellMetachar(command string) bool { return hasShellMetachar(command) }

// ShlexSplit is shlex.split(s) (posix mode); err is non-nil where Python
// raises ValueError.
func ShlexSplit(s string) ([]string, error) { return posixSplit(s) }

// ShlexQuote is shlex.quote(s).
func ShlexQuote(s string) string { return shlexQuote(s) }

// Normpath is posixpath.normpath.
func Normpath(p string) string { return posixNormpath(p) }

// HasGlobChars is verify._GLOB_CHARS_RE.search.
func HasGlobChars(s string) bool { return hasGlobChars(s) }

// IsWriter reports whether a command head is one of write_paths.WRITERS.
func IsWriter(head string) bool { _, ok := writers[head]; return ok }

// Vacuity is vacuity.check_vacuity on the process's Z3, for a caller
// outside the predicate stage (the alias registry). Unlike the stage, it
// returns the error.
func Vacuity(expr Expression, timeoutMs int) (VacuityVerdict, error) {
	z3c, err := sharedZ3()
	if err != nil {
		return NonTrivial, err
	}
	return CheckVacuity(z3c, expr, timeoutMs)
}
