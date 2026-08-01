package verify

import "strings"

// hasShellMetachar ports verify._SHELL_METACHAR_RE =
// re.compile(r"[;|&`<>\n\r]|\$\(") — a hand-written scan instead of Go's
// regexp package since this runs on every shell command in the corpus and a
// byte scan is both simpler to get exactly right and faster than compiling
// (even a cached) regexp for a fixed 9-symbol alternation.
func hasShellMetachar(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ';', '|', '&', '`', '<', '>', '\n', '\r':
			return true
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
		}
	}
	return false
}

// isEnvAssignToken ports verify._ENV_ASSIGN_RE =
// re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=").
func isEnvAssignToken(tok string) bool {
	if tok == "" {
		return false
	}
	c := tok[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_') {
		return false
	}
	i := 1
	for i < len(tok) {
		c = tok[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			i++
			continue
		}
		break
	}
	return i < len(tok) && tok[i] == '='
}

// extractShellHead ports verify._extract_shell_head. Returns ("", false) for
// lines that execute nothing (blank, comment-only, env-assignments only).
func extractShellHead(stripped string) (string, bool) {
	if stripped == "" {
		return "", false
	}
	if strings.HasPrefix(stripped, "#") {
		return "", false
	}
	for _, tok := range strings.Fields(stripped) {
		if isEnvAssignToken(tok) {
			continue
		}
		return tok, true
	}
	return "", false
}
