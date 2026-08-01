package verify

import "strings"

// posixSplitRoot ports posixpath.splitroot (Python 3.12): returns the root
// ("" / "/" / "//" — POSIX gives exactly-two-leading-slashes special
// treatment) and the tail after it.
func posixSplitRoot(p string) (root, tail string) {
	if len(p) == 0 || p[0] != '/' {
		return "", p
	}
	if len(p) < 2 || p[1] != '/' || (len(p) >= 3 && p[2] == '/') {
		return "/", p[1:]
	}
	return "//", p[2:]
}

// posixNormpath ports posixpath.normpath EXACTLY (including the quirk that a
// leading ".." on a relative path is kept, since there's nothing to pop).
func posixNormpath(p string) string {
	if p == "" {
		return "."
	}
	root, tail := posixSplitRoot(p)
	comps := strings.Split(tail, "/")
	newComps := make([]string, 0, len(comps))
	for _, comp := range comps {
		if comp == "" || comp == "." {
			continue
		}
		isDotDot := comp == ".."
		keep := !isDotDot ||
			(root == "" && len(newComps) == 0) ||
			(len(newComps) > 0 && newComps[len(newComps)-1] == "..")
		if keep {
			newComps = append(newComps, comp)
		} else if len(newComps) > 0 {
			newComps = newComps[:len(newComps)-1]
		}
	}
	result := root + strings.Join(newComps, "/")
	if result == "" {
		return "."
	}
	return result
}

// matchGlob ports verify._match_glob: a LEFT-anchored, "/"-aware glob match
// against an already posixpath.normpath'd path.
//
// ADJUDICATIONS.md F-1/F-2 — FIXED IN THE ORACLE, this file rewritten (not
// patched) to match. The old matcher delegated to PurePosixPath(path).match,
// which is RIGHT-anchored for a relative pattern: file_write=["out.txt"]
// admitted /etc/cron.d/out.txt (a scope escape), and a mid-pattern "**"
// changed meaning between Python 3.12 (a plain one-segment wildcard
// component) and 3.13 (recursive). The oracle now implements its own
// native matcher instead: a pattern must consume the WHOLE path (left- AND
// right-anchored — i.e. anchored on both ends, same segment count except
// where "**" spans), so a relative pattern can never match an absolute
// path by accident, and "**" recursively spans zero or more segments the
// same way on every Python version. "*"/"?"/"[...]" stay within one
// segment (via fnmatchCase). This port is a direct, line-for-line
// translation of the oracle's `_match_glob` — see verify.py for the
// canonical source (read-only reference, never edited).
func matchGlob(norm, glob string) bool {
	if strings.HasSuffix(glob, "/**") {
		raw := glob[:len(glob)-3]
		if raw == "" {
			// "/**" — the root: any absolute path.
			return strings.HasPrefix(norm, "/")
		}
		prefix := posixNormpath(raw)
		if prefix == "." {
			// "./**" — any relative path (never an absolute one, and not
			// a path that climbs above the starting directory).
			return !strings.HasPrefix(norm, "/") && norm != ".." && !strings.HasPrefix(norm, "../")
		}
		return norm == prefix || strings.HasPrefix(norm, prefix+"/")
	}

	// Plain split — do NOT filter empty components: an absolute path/
	// pattern keeps its leading "" segment (from the leading "/"), which is
	// exactly what anchors an absolute pattern to an absolute path and
	// rejects a relative one (their segment lists can never align).
	patSegs := strings.Split(glob, "/")
	pathSegs := strings.Split(norm, "/")

	var matchFrom func(pi, ti int) bool
	matchFrom = func(pi, ti int) bool {
		if pi == len(patSegs) {
			return ti == len(pathSegs)
		}
		if patSegs[pi] == "**" {
			for k := ti; k <= len(pathSegs); k++ {
				if matchFrom(pi+1, k) {
					return true
				}
			}
			return false
		}
		if ti == len(pathSegs) {
			return false
		}
		if fnmatchCase(pathSegs[ti], patSegs[pi], false) {
			return matchFrom(pi+1, ti+1)
		}
		return false
	}
	return matchFrom(0, 0)
}

// PathMatchesAny is verify._path_matches_any: normalize the path once, then
// try every glob.
func PathMatchesAny(path string, globs []string) bool {
	normalized := posixNormpath(path)
	for _, g := range globs {
		if matchGlob(normalized, g) {
			return true
		}
	}
	return false
}
