package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/wardenroot"
)

// ErrUnsupported marks a settings file this package does not edit: text
// pyjson does not model (a lone surrogate), or a shape Python.s reverse
// raises on after its backup is made. The whole command is refused
// before anything is written.
var ErrUnsupported = errors.New("a settings file holds a shape this binary does not edit yet")

// FailError is a shape Python's apply raises on: the runtime fails,
// nothing is written for it, and the CLI names it and exits 1.
type FailError struct{ Why string }

func (e *FailError) Error() string { return e.Why }

func failf(format string, a ...any) error { return &FailError{Why: fmt.Sprintf(format, a...)} }

func isFail(err error) bool {
	var f *FailError
	return errors.As(err, &f)
}

// Edit is one planned change: a file written (after a backup when it
// existed), or a warning with no write.
type Edit struct {
	Path    string
	Existed bool
	Content string
	Warning string
	// PreWarning is said before the write, which still happens.
	PreWarning string
	// Kind is "" for a file written; else an edit of layers.go ("skill",
	// "plugin", "md", "remove", "md-unpatch").
	Kind string
	// Listed is an edit reported among the files written even when it
	// changes nothing (the skill, as _link_skill returns its target).
	Listed bool
	// Note is a line said after the file list: a warden hook stepped up
	// from audit to enforce, or kept in enforce.
	Note string
	// NoteOnly is an edit that only carries Note: it writes nothing and
	// is not listed.
	NoteOnly bool
	// Undo marks an edit of an uninstall (PlanReverseAll sets it, with
	// Home). A file it leaves empty, or holding only {}, is removed, and
	// so is each directory above a removed path that is left empty, up to
	// a harness root or Home: uninstall leaves the config as it found it.
	Undo bool
	Home string
}

// readState is how Python's json.loads(path.read_text()) went.
type readState int

const (
	readOK      readState = iota
	readMissing           // path.exists() is false
	readBad               // JSONDecodeError or OSError: caught, warned
)

func readJSON(p string) (any, readState, error) {
	if _, err := os.Stat(p); err != nil {
		return nil, readMissing, nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, readBad, nil
	}
	if !utf8.Valid(raw) {
		// UnicodeDecodeError is not caught: the runtime fails.
		return nil, readOK, failf("%s is not valid UTF-8", p)
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		if errors.Is(err, pyjson.ErrUnsupported) {
			return nil, readOK, ErrUnsupported
		}
		return nil, readBad, nil
	}
	return v, readOK, nil
}

// iterItems is Python's `for x in v` where v came from JSON, for a loop
// whose body calls x.get: a list yields its items, an empty object or
// string yields nothing, anything else raises.
func iterItems(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case *pyjson.Object:
		if x.Len() == 0 {
			return nil, nil
		}
	case string:
		if x == "" {
			return nil, nil
		}
	}
	return nil, failf("a hooks value is a %s, not a list", kind(v))
}

func kind(v any) string {
	switch v.(type) {
	case *pyjson.Object:
		return "mapping"
	case []any:
		return "list"
	case string:
		return "string"
	case nil:
		return "null"
	}
	return "scalar"
}

// commandHooks returns every hook of type "command" under entries, in
// order, as _patch_claude_warden and _patch_codex_warden walk them.
func commandHooks(entries []any) ([]*pyjson.Object, error) {
	var out []*pyjson.Object
	for _, e := range entries {
		entry, isObj := e.(*pyjson.Object)
		if !isObj {
			return nil, failf("a PreToolUse entry is a %s, not a mapping", kind(e))
		}
		hv, present := entry.Get("hooks")
		if !present {
			continue
		}
		hs, err := iterItems(hv)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			hook, isObj := h.(*pyjson.Object)
			if !isObj {
				return nil, failf("a hook is a %s, not a mapping", kind(h))
			}
			if t, _ := hook.Get("type"); t == "command" {
				out = append(out, hook)
			}
		}
	}
	return out, nil
}

// preToolUse is settings.setdefault("hooks", {}).setdefault("PreToolUse", []).
func preToolUse(settings any) (*pyjson.Object, []any, error) {
	s, isObj := settings.(*pyjson.Object)
	if !isObj {
		return nil, nil, failf("the file holds a %s, not a mapping", kind(settings))
	}
	hv, present := s.Get("hooks")
	if !present {
		hv = pyjson.NewObject()
		s.Set("hooks", hv)
	}
	hooks, isObj := hv.(*pyjson.Object)
	if !isObj {
		return nil, nil, failf("\"hooks\" is a %s, not a mapping", kind(hv))
	}
	pv, present := hooks.Get("PreToolUse")
	if !present {
		pv = []any{}
		hooks.Set("PreToolUse", pv)
	}
	pre, isList := pv.([]any)
	if !isList {
		// Iterating or appending to anything else raises.
		return nil, nil, failf("\"PreToolUse\" is a %s, not a list", kind(pv))
	}
	return hooks, pre, nil
}

func dumpSettings(v any) string { return pyjson.DumpsIndent(v, 2, true) + "\n" }

// PlanClaudeWarden is _patch_claude_warden with the Go hook command: nil when
// the exact hook is already installed, a warning when another warden hook
// is, else the new settings.json. Python's in-place upgrade of an old
// `-m opendaisugi.warden` hook to warden_client never matches the Go command,
// so such a hook is warned about and left as it is.
func PlanClaudeWarden(settingsPath string, entry *pyjson.Object) (*Edit, error) {
	return PlanClaudeWardenOn(settingsPath, entry, nil)
}

// PlanClaudeWardenOn is PlanClaudeWarden on the content an earlier edit of
// this run left (prior), or the file.
func PlanClaudeWardenOn(settingsPath string, entry *pyjson.Object, prior *Edit) (*Edit, error) {
	cmdV, _ := entry.Value("hooks").([]any)[0].(*pyjson.Object).Get("command")
	wardenCommand := cmdV.(string)
	v, st, err := jsonOf(settingsPath, prior)
	if err != nil {
		return nil, err
	}
	existed := st != readMissing
	if st == readBad {
		return &Edit{Path: settingsPath, Warning: settingsPath + " is not valid JSON; skipping warden hook installation " +
			"to avoid overwriting your Claude Code settings (permissions/env). " +
			"Fix the file and re-run `daisugi install`."}, nil
	}
	if st == readMissing {
		v = pyjson.NewObject()
	}
	hooks, pre, err := preToolUse(v)
	if err != nil {
		return nil, err
	}
	cmds, err := commandHooks(pre)
	if err != nil {
		return nil, err
	}
	var existing []any
	for _, h := range cmds {
		c, present := h.Get("command")
		if !present {
			return nil, failf("a hook of type \"command\" has no \"command\"") // h["command"]
		}
		switch c.(type) {
		case []any, *pyjson.Object:
			return nil, failf("a hook command is a %s", kind(c)) // unhashable in a set
		}
		existing = append(existing, c)
	}
	for _, c := range existing {
		if c == wardenCommand {
			return nil, nil
		}
	}
	anyWarden := false
	for _, c := range existing {
		if config.WardenHookKind(c) != config.KindNone {
			anyWarden = true
		}
	}
	if anyWarden {
		// Audit to enforce, nothing else different, is the one step
		// `daisugi install --enforce` asks for: the hook is rewritten in
		// place. Enforce is never lowered by a plain install.
		stepped, kept := false, false
		newHook, _ := entry.Value("hooks").([]any)[0].(*pyjson.Object)
		for _, h := range cmds {
			old, _ := h.Value("command").(string)
			switch WardenModeRelation(old, wardenCommand) {
			case "up":
				h.Set("command", wardenCommand)
				h.Set("timeout", newHook.Value("timeout"))
				stepped = true
			case "keep":
				kept = true
			}
		}
		if stepped {
			return &Edit{Path: settingsPath, Existed: existed, Content: dumpSettings(v),
				Note: wardenStepUpNote("Claude Code", settingsPath)}, nil
		}
		if kept {
			return &Edit{Path: settingsPath, NoteOnly: true, Note: wardenKeepNote("Claude Code", settingsPath)}, nil
		}
	}
	for _, c := range existing {
		if config.WardenHookKind(c) != config.KindNone {
			return &Edit{Path: settingsPath, Warning: "a warden hook is already installed in " + settingsPath +
				" with a different mode/config; run `daisugi install --uninstall` first if you want to " +
				"change it (idempotent by presence, not content)." + notIsolatedNote(existing)}, nil
		}
	}
	hooks.Set("PreToolUse", append(pre, entry))
	return &Edit{Path: settingsPath, Existed: existed, Content: dumpSettings(v)}, nil
}

// PlanCodexWarden is _patch_codex_warden: the warden entry with matcher ".*",
// added unless any warden hook is there already.
func PlanCodexWarden(hooksPath string, entry *pyjson.Object) (*Edit, error) {
	entry.Set("matcher", ".*")
	v, st, err := readJSON(hooksPath)
	if err != nil {
		return nil, err
	}
	existed := st != readMissing
	if st == readBad {
		return &Edit{Path: hooksPath, Warning: hooksPath + " is not valid JSON; skipping warden hook installation. " +
			"Fix the file and re-run `daisugi install`."}, nil
	}
	if st == readMissing {
		v = pyjson.NewObject()
	}
	hooks, pre, err := preToolUse(v)
	if err != nil {
		return nil, err
	}
	cmds, err := commandHooks(pre)
	if err != nil {
		return nil, err
	}
	for _, h := range cmds {
		switch h.Value("command").(type) {
		case []any, *pyjson.Object:
			return nil, failf("a hook command is a %s", kind(h.Value("command"))) // unhashable in a set
		}
	}
	newCommand, _ := entry.Value("hooks").([]any)[0].(*pyjson.Object).Value("command").(string)
	anyWarden, stepped, kept := false, false, false
	for _, h := range cmds {
		if config.WardenHookKind(h.Value("command")) != config.KindNone {
			anyWarden = true
			old, _ := h.Value("command").(string)
			switch WardenModeRelation(old, newCommand) {
			case "up":
				h.Set("command", newCommand)
				stepped = true
			case "keep":
				kept = true
			}
		}
	}
	if anyWarden {
		if stepped {
			return &Edit{Path: hooksPath, Existed: existed, Content: dumpSettings(v),
				Note: wardenStepUpNote("Codex", hooksPath)}, nil
		}
		if kept {
			return &Edit{Path: hooksPath, NoteOnly: true, Note: wardenKeepNote("Codex", hooksPath)}, nil
		}
		return nil, nil
	}
	hooks.Set("PreToolUse", append(pre, entry))
	return &Edit{Path: hooksPath, Existed: existed, Content: dumpSettings(v)}, nil
}

// PlanPopHook is _pop_json_hook: every hook whose command match accepts
// removed from the given events, empty entries and events dropped. prior
// is an earlier edit of the same file in this run, whose content is read
// instead of the file.
func PlanPopHook(settingsPath string, match func(any) bool, events []string, prior *Edit) (*Edit, error) {
	var v any
	if prior != nil && prior.Content != "" {
		var err error
		if v, err = pyjson.Loads(prior.Content); err != nil {
			return nil, err
		}
	} else {
		var st readState
		var err error
		v, st, err = readJSON(settingsPath)
		if err != nil {
			return nil, err
		}
		if st != readOK {
			return nil, nil
		}
	}
	s, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, failf("the file holds a %s, not a mapping", kind(v))
	}
	hv, present := s.Get("hooks")
	if !present {
		hv = pyjson.NewObject()
	}
	hooks, isObj := hv.(*pyjson.Object)
	if !isObj {
		return nil, failf("\"hooks\" is a %s, not a mapping", kind(hv))
	}
	// The any() over events, entries and hooks, in order: it stops at the
	// first match, so a bad shape after it is not reached.
	found := false
scan:
	for _, ev := range events {
		ents := hooks.Value(ev)
		if !pyjson.Truthy(ents) {
			continue
		}
		list, isList := ents.([]any)
		if !isList {
			return nil, failf("%q is a %s, not a list", ev, kind(ents))
		}
		for _, e := range list {
			entry, isObj := e.(*pyjson.Object)
			if !isObj {
				return nil, failf("a %s entry is a %s, not a mapping", ev, kind(e))
			}
			hsV, present := entry.Get("hooks")
			if !present {
				continue
			}
			hs, err := iterItems(hsV)
			if err != nil {
				return nil, err
			}
			for _, h := range hs {
				hook, isObj := h.(*pyjson.Object)
				if !isObj {
					return nil, failf("a hook is a %s, not a mapping", kind(h))
				}
				if match(hook.Value("command")) {
					found = true
					break scan
				}
			}
		}
	}
	if !found {
		return nil, nil
	}
	// The rewrite walks every entry of every event. A shape it would
	// raise on here fails after the backup was made: refused instead.
	for _, ev := range events {
		ents := hooks.Value(ev)
		if !pyjson.Truthy(ents) {
			continue
		}
		list, isList := ents.([]any)
		if !isList {
			return nil, ErrUnsupported
		}
		var kept []any
		for _, e := range list {
			entry, isObj := e.(*pyjson.Object)
			if !isObj {
				return nil, ErrUnsupported
			}
			var hs []any
			if hsV, present := entry.Get("hooks"); present {
				var err error
				if hs, err = iterItems(hsV); err != nil {
					return nil, ErrUnsupported
				}
			}
			left := []any{}
			for _, h := range hs {
				hook, isObj := h.(*pyjson.Object)
				if !isObj {
					return nil, ErrUnsupported
				}
				if !match(hook.Value("command")) {
					left = append(left, h)
				}
			}
			entry.Set("hooks", left)
			if len(left) > 0 {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			hooks.Delete(ev)
		} else {
			hooks.Set(ev, kept)
		}
	}
	if hooks.Len() == 0 {
		s.Delete("hooks")
	}
	return &Edit{Path: settingsPath, Existed: true, Content: dumpSettings(s)}, nil
}

// Backup is install._backup: a copy with the mode and times kept, named
// <name>.bak<ns>, never over an existing backup. The copy has its final
// mode before any content is written into it.
func Backup(p string) error {
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	dest := p + ".bak" + stamp
	for n := 1; ; n++ {
		if _, err := os.Lstat(dest); err != nil {
			break
		}
		dest = p + ".bak" + stamp + "." + strconv.Itoa(n)
	}
	src, err := os.Open(p)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	// A backup that could not be written whole is removed: a partial copy
	// must never pass for the file it was made from.
	if err := out.Chmod(st.Mode().Perm()); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return os.Chtimes(dest, st.ModTime(), st.ModTime())
}

// Apply performs a planned edit: a backup of the file as it stands when
// it existed, then the write. Two edits of one file are applied in order,
// so each backup holds what the earlier edit left, as in Python. followed
// is the file written when Path was a symlink.
func Apply(e *Edit) (followed string, err error) { return applyEdit(e, nil) }

// applyEdit is Apply with the files this run has already backed up: a file
// gets one backup per run, of the file as the run found it. nil backs up on
// every edit.
func applyEdit(e *Edit, backed map[string]bool) (followed string, err error) {
	if e == nil || e.Warning != "" || e.NoteOnly {
		return "", nil
	}
	if e.Undo {
		defer func() {
			if err == nil {
				pruneEmptyParents(e.Path, e.Home)
			}
		}()
	}
	if e.Kind != "" {
		return applyLayerEdit(e, backed)
	}
	// An undo that empties the file removes it with no backup: what is
	// left is daisugi's own text, or nothing.
	if e.Undo && emptyContent(e.Content) && isRegular(e.Path) {
		return "", os.Remove(e.Path)
	}
	if e.Existed {
		if err := backupOnce(e.Path, backed); err != nil {
			return "", fmt.Errorf("backup %s: %w", e.Path, err)
		}
	} else if backed != nil {
		// This run makes the file: a later edit of it in the same run
		// would back up only daisugi's own half-write.
		backed[e.Path] = true
	}
	content := e.Content
	if e.Existed {
		content = keepFileForm(e.Path, content)
	}
	return wardenroot.WriteFile(e.Path, content)
}

// backupOnce is Backup, skipped for a file this run has already backed up.
func backupOnce(p string, backed map[string]bool) error {
	if backed != nil && backed[p] {
		return nil
	}
	if err := Backup(p); err != nil {
		return err
	}
	if backed != nil {
		backed[p] = true
	}
	return nil
}

// emptyContent is a file an uninstall leaves with nothing in it: no text,
// or a JSON object with no keys.
func emptyContent(c string) bool {
	t := strings.TrimSpace(c)
	return t == "" || t == "{}"
}

func isRegular(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular()
}

// keepFileForm keeps two things of the file a rewrite replaces, so an
// edit and its undo give back the same bytes: UTF-8 text stays UTF-8 (a
// JSON rewrite escapes every non-ASCII rune, and Claude Code writes
// ~/.claude.json with JSON.stringify, which does not), and a file with no
// final newline gets none.
func keepFileForm(p, content string) string {
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return content
	}
	if hasNonASCII(b) && strings.HasPrefix(strings.TrimSpace(content), "{") {
		if v, err := pyjson.Loads(content); err == nil {
			content = pyjson.DumpsIndent(v, 2, false) + "\n"
		}
	}
	if b[len(b)-1] != '\n' {
		content = strings.TrimSuffix(content, "\n")
	}
	return content
}

func hasNonASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}

// harnessRoots are the directories an uninstall never removes, even when
// it leaves them empty: each harness's own root, and the shared homes.
var harnessRoots = []string{
	".claude", ".codex", ".hermes", ".openclaw", ".pi", ".pi/agent",
	".config", ".config/opencode", ".local", ".local/share", ".local/state", ".cache",
}

// pruneEmptyParents removes each directory above p that is empty, going
// up, and stops at the first one that is not empty, at a harness root, at
// home, or at a symlink: os.Remove would unlink a link whatever its target
// holds, so only a real directory is ever removed.
func pruneEmptyParents(p, home string) {
	if home == "" {
		return
	}
	home = filepath.Clean(home)
	stop := map[string]bool{home: true}
	for _, r := range harnessRoots {
		stop[filepath.Join(home, r)] = true
	}
	for dir := filepath.Dir(filepath.Clean(p)); strings.HasPrefix(dir, home+"/") && !stop[dir]; dir = filepath.Dir(dir) {
		if !removeEmptyDir(dir) {
			return
		}
	}
}

// removeEmptyDir removes dir when it is a real, empty directory, and says
// whether it did. A symlink to a directory is never removed.
func removeEmptyDir(dir string) bool {
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	return os.Remove(dir) == nil
}

var (
	wardenModeRe = regexp.MustCompile(` --mode (audit|enforce) `)
	wardenAskRe  = regexp.MustCompile(` --ask --ask-timeout \d+`)
)

// wardenShape is a warden command without its mode, its ask words and its
// enforce tail: two commands with the same shape differ only in those.
func wardenShape(command string) string {
	shape := strings.TrimSuffix(command, " || exit 2")
	shape = wardenAskRe.ReplaceAllString(shape, "")
	if loc := wardenModeRe.FindStringIndex(shape); loc != nil {
		shape = shape[:loc[0]] + " --mode _ " + shape[loc[1]:]
	}
	return shape
}

func wardenModeOf(command string) string {
	if m := wardenModeRe.FindStringSubmatch(command); m != nil {
		return m[1]
	}
	return ""
}

// WardenModeRelation is how an installed warden command relates to the one a
// run asks for when only the mode (and the ask words that go with enforce)
// differ: "up" for audit installed and enforce asked, "keep" for enforce
// installed and audit asked, else "".
func WardenModeRelation(oldCommand, newCommand string) string {
	if wardenShape(oldCommand) != wardenShape(newCommand) {
		return ""
	}
	switch wardenModeOf(oldCommand) + ">" + wardenModeOf(newCommand) {
	case "audit>enforce":
		return "up"
	case "enforce>audit":
		return "keep"
	}
	return ""
}

func wardenStepUpNote(runtime, path string) string {
	return "Warden: the " + runtime + " hook in " + path + " now runs in enforce mode (it was audit)."
}

func wardenKeepNote(runtime, path string) string {
	return "Warden: the " + runtime + " hook in " + path + " stays in enforce mode; a plain install never " +
		"lowers it. To go back to audit, run daisugi install --uninstall, then daisugi install."
}
