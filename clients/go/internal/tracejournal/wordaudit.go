package tracejournal

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pyyaml"
)

// WordWouldDeny is journal.word_would_deny: the traces whose verification
// result holds a warning that starts with auditPrefix (the dialect audit).
// It never fails: a missing journal counts 0, and a trace it cannot read,
// that is not UTF-8, or whose result.warnings is not a list is skipped. A
// trace whose text does not hold the word dialect is not loaded. A trace
// the port's reader does not handle (not in the form yaml.safe_dump
// writes) is skipped too.
func WordWouldDeny(dataDir, auditPrefix string) int {
	dir := filepath.Join(dataDir, "journal", "traces")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	count := 0
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !utf8.Valid(raw) {
			continue
		}
		text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
		if !strings.Contains(text, "dialect") {
			continue
		}
		v, why := pyyaml.LoadDumped(text)
		if why != nil {
			continue
		}
		o, isObj := v.(*pyjson.Object)
		if !isObj {
			continue
		}
		result, isObj := o.Value("result").(*pyjson.Object)
		if !isObj {
			continue
		}
		warnings, isList := result.Value("warnings").([]any)
		if !isList {
			continue
		}
		for _, w := range warnings {
			if s, isStr := w.(string); isStr && strings.HasPrefix(s, auditPrefix) {
				count++
				break
			}
		}
	}
	return count
}
