package cli

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/viz"
)

// vizCmd is `daisugi viz [PATHWAY_ID] [--data-dir D] [-o FILE]`: a
// distilled pathway's plan as a standalone execution-monitor page, or the
// list of pathways to pick from.
func (e *Env) vizCmd(args []string) error {
	const cmd = "viz"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory (pathway store)."},
		{names: []string{"--output", "-o"}, value: true, metavar: "PATH",
			help: "Write the HTML here (default: <pathway_id>.html)."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " [PATHWAY_ID]", "Render a distilled pathway's plan as a standalone execution-monitor page.", opts)
	}
	dataDir := e.dataDir(p)
	db := gateroot.Join(dataDir, "pathways.db")
	if _, err := os.Stat(db); err != nil {
		e.errf("No pathway store at %s. Run `daisugi onboard` or `daisugi tend` first.\n", db)
		return exit(1)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	id := ""
	if len(p.args) > 0 {
		id = p.args[0]
	}
	if id == "" {
		all, err := s.ReadAll(nil)
		if err != nil {
			return e.storeErr(cmd, err)
		}
		if len(all) == 0 {
			e.out("No pathways distilled yet. Run `daisugi onboard` or `daisugi tend`.\n")
			return nil
		}
		var b strings.Builder
		b.WriteString(itoa64(int64(len(all))) + " pathway(s) — pass an id to `daisugi viz`:\n")
		for _, pw := range all {
			b.WriteString("  " + pw.ID() + "  " + pystr.Slice(pw.Task(), 0, 64) + "\n")
		}
		e.out("%s", b.String())
		return nil
	}
	all, err := s.ReadAll(func(x string) bool { return x == id })
	if err != nil {
		return e.storeErr(cmd, err)
	}
	var obj *pyjson.Object
	for _, pw := range all {
		if pw.ID() == id {
			obj = pw.Obj
		}
	}
	if obj == nil {
		e.errf("No pathway %s in %s. Run `daisugi viz` to list.\n", pystr.Repr(id), db)
		return exit(1)
	}
	env, _ := obj.Value("envelope").(*pyjson.Object)
	plan, _ := obj.Value("plan_template").(*pyjson.Object)
	if env == nil || plan == nil {
		return e.refuse(cmd, errString("the pathway's envelope or plan is not one this binary reads"))
	}
	// Title the page with the distilled task description.
	env2 := pyjson.NewObject()
	for _, k := range env.Keys() {
		env2.Set(k, env.Value(k))
	}
	env2.Set("task", obj.Value("task_description"))
	html, err := viz.Render(plan, env2)
	if err != nil {
		return e.refuse(cmd, err)
	}
	out := id + ".html"
	if p.has("--output") {
		out = p.str("--output", "")
	}
	out = gateroot.PathStr(out)
	if err := os.WriteFile(out, []byte(html), 0o666); err != nil {
		var no syscall.Errno
		if !errors.As(err, &no) {
			return e.failPy(cmd, err)
		}
		name := map[syscall.Errno]string{syscall.ENOENT: "FileNotFoundError", syscall.EISDIR: "IsADirectoryError",
			syscall.ENOTDIR: "NotADirectoryError", syscall.EACCES: "PermissionError", syscall.EPERM: "PermissionError"}[no]
		if name == "" {
			name = "OSError"
		}
		e.errf("Traceback (most recent call last): ...\n%s: [Errno %d] %s: %s\n", name, int(no), pyStrerror(no), pystr.Repr(out))
		return exit(1)
	}
	e.out("Wrote %s (%d bytes) — open it in a browser.\n", out, utf8.RuneCountInString(html))
	return nil
}

type errString string

func (s errString) Error() string { return string(s) }
