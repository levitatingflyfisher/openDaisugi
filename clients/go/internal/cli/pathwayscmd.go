package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

var pathwaysHelp = `Usage: daisugi pathways [OPTIONS] COMMAND [ARGS]...

  Manage compiled pathways.

Commands:
  list     List all compiled pathways.
  show     Show a compiled pathway in detail.
  stats    Summarize stored pathways (count, total hits).
  delete   Delete a compiled pathway.
  export   Export a compiled pathway for sharing or inspection.
  import   Import a pathway bundle, re-verify, and admit to the PathwayStore.
`

var dataDirOpt = opt{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."}

func (e *Env) pathways(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", pathwaysHelp)
		return nil
	}
	// A store is read whole, as Python reads it; collecting less often
	// saves a tenth of the time on a large one.
	debug.SetGCPercent(400)
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return e.pathwaysList(rest)
	case "show":
		return e.pathwaysShow(rest)
	case "stats":
		return e.pathwaysStats(rest)
	case "delete":
		return e.pathwaysDelete(rest)
	case "export":
		return e.pathwaysExport(rest)
	case "import":
		return e.pathwaysImport(rest)
	}
	e.errf("Usage: daisugi pathways [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi pathways --help' for help.\n\nError: No such command '%s'.\n", sub)
	return exit(2)
}

// missingArg is click's error for an absent required argument.
func (e *Env) missingArg(cmd, usageArgs, name string) error {
	e.errf("Usage: daisugi %s [OPTIONS] %s\nTry 'daisugi %s --help' for help.\n\nError: Missing argument '%s'.\n",
		cmd, usageArgs, cmd, strings.ToLower(name))
	return exit(2)
}

// openStore is PathwayStore(data_dir / "pathways.db").
func (e *Env) openStore(cmd string, p *parsed) (*pathways.Store, error) {
	dir := p.str("--data-dir", filepath.Join(e.home, ".opendaisugi"))
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		// Path.mkdir(parents=True, exist_ok=True) on a file.
		e.errf("daisugi %s: FileExistsError: %s exists and is not a directory\n", cmd, dir)
		return nil, exit(1)
	}
	s, err := pathways.Open(filepath.Join(dir, "pathways.db"))
	if errors.Is(err, pathways.ErrPath) {
		return nil, e.refuse(cmd, err)
	}
	if err != nil {
		return nil, e.failPy(cmd, err)
	}
	return s, nil
}

// storeErr ends a command on an error from the store.
func (e *Env) storeErr(cmd string, err error) error {
	if errors.Is(err, pathways.ErrUnreadable) {
		return e.refuse(cmd, err)
	}
	return e.failPy(cmd, err)
}

func (e *Env) pathwaysList(args []string) error {
	const cmd = "pathways list"
	opts := []opt{dataDirOpt, jsonOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "List all compiled pathways.", opts)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	all, err := s.ReadAll(nil)
	if err != nil {
		return e.storeErr(cmd, err)
	}
	if p.flag("--json") {
		items := []any{}
		for _, pw := range all {
			o := pw.Obj
			items = append(items, pyjson.NewObject().
				Set("id", o.Value("id")).
				Set("task_description", o.Value("task_description")).
				Set("hit_count", o.Value("hit_count")).
				Set("version", o.Value("version")).
				Set("distilled_at", o.Value("distilled_at")))
		}
		e.out("%s\n", pyjson.Dumps(pathways.JSONMode(items), true))
		return nil
	}
	if len(all) == 0 {
		e.out("No compiled pathways.\n")
		return nil
	}
	var b strings.Builder
	for _, pw := range all {
		o := pw.Obj
		b.WriteString(pw.ID() + "  hits=" + o.Value("hit_count").(pyjson.Int).Text + "  v" +
			o.Value("version").(pyjson.Int).Text + "  " + pw.Task() + "\n")
	}
	e.out("%s", b.String())
	return nil
}

func (e *Env) pathwaysShow(args []string) error {
	const cmd = "pathways show"
	opts := []opt{dataDirOpt, jsonOpt}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PATHWAY_ID", "Show a compiled pathway in detail.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "PATHWAY_ID", "PATHWAY_ID")
	}
	id := p.args[0]
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	all, err := s.ReadAll(func(x string) bool { return x == id })
	if err != nil {
		return e.storeErr(cmd, err)
	}
	for _, pw := range all {
		if pw.ID() != id {
			continue
		}
		if p.flag("--json") {
			o := pyjson.NewObject()
			for _, k := range pw.Obj.Keys() {
				if k != "task_embedding" {
					o.Set(k, pw.Obj.Value(k))
				}
			}
			e.out("%s\n", pyjson.Dumps(pathways.JSONMode(o), true))
			return nil
		}
		e.out("%s\n", pathways.DumpJSONIndent(pw.Full().Obj, 2))
		return nil
	}
	e.errf("Pathway %s not found.\n", pystr.Repr(id))
	return exit(1)
}

func (e *Env) pathwaysStats(args []string) error {
	const cmd = "pathways stats"
	opts := []opt{dataDirOpt, {names: []string{"--json"}, help: "Emit stats as JSON."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Summarize stored pathways (count, total hits).", opts)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	count, hits, err := s.Stats()
	if err != nil {
		return e.failPy(cmd, err)
	}
	var hv any
	switch h := hits.(type) {
	case int64:
		hv = pyjson.Int{Text: itoa64(h)}
	case float64:
		hv = h
	default:
		return e.refuse(cmd, errors.New("SUM(hit_count) is neither an int nor a float"))
	}
	if p.flag("--json") {
		o := pyjson.NewObject().Set("count", pyjson.Int{Text: itoa64(count)}).Set("total_hits", hv)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	e.out("count: %d\ntotal_hits: %s\n", count, pyjson.Dumps(hv, true))
	return nil
}

func (e *Env) pathwaysDelete(args []string) error {
	const cmd = "pathways delete"
	opts := []opt{dataDirOpt}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PATHWAY_ID", "Delete a compiled pathway.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "PATHWAY_ID", "PATHWAY_ID")
	}
	id := p.args[0]
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	ok, err := s.Delete(id)
	if err != nil {
		return e.failPy(cmd, err)
	}
	if !ok {
		e.errf("Pathway %s not found.\n", pystr.Repr(id))
		return exit(1)
	}
	e.out("Deleted %s.\n", id)
	return nil
}

func (e *Env) pathwaysExport(args []string) error {
	const cmd = "pathways export"
	opts := []opt{{names: []string{"--format"}, value: true, metavar: "TEXT", help: "Export format: json, skill, mermaid, md, smtlib."}, dataDirOpt}
	p, err := parseArgs(args, opts, 2)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PATHWAY_ID OUTPUT", "Export a compiled pathway for sharing or inspection.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "PATHWAY_ID OUTPUT", "PATHWAY_ID")
	}
	if len(p.args) < 2 {
		return e.missingArg(cmd, "PATHWAY_ID OUTPUT", "OUTPUT")
	}
	id, output := p.args[0], p.args[1]
	format := p.str("--format", "skill")
	known := false
	for _, f := range pathways.Formats {
		known = known || f == format
	}
	if !known {
		e.errf("Unknown format %s. Supported: %s.\n", pystr.Repr(format), strings.Join(pathways.Formats, ", "))
		return exit(2)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	all, err := s.ReadAll(func(x string) bool { return x == id })
	if err != nil {
		return e.storeErr(cmd, err)
	}
	var match *pathways.Pathway
	for _, pw := range all {
		if pw.ID() == id {
			match = pw
			break
		}
	}
	if match == nil {
		e.errf("Pathway %s not found.\n", pystr.Repr(id))
		return exit(1)
	}
	text, err := pathways.Export(match.Full(), format, version())
	if err != nil {
		return e.storeErr(cmd, err)
	}
	out := gateroot.PathStr(output)
	if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
		return e.failPy(cmd, err)
	}
	if err := os.WriteFile(out, []byte(text), 0o666); err != nil {
		return e.failPy(cmd, err)
	}
	e.out("Exported %s → %s (%s)\n", id, out, format)
	return nil
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func (e *Env) pathwaysImport(args []string) error {
	const cmd = "pathways import"
	opts := []opt{dataDirOpt,
		{names: []string{"--overwrite"}, help: "Replace an existing pathway with the same ID."},
		{names: []string{"--z3-timeout-ms"}, value: true, metavar: "INTEGER", help: "Z3 timeout for the re-verification."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " SOURCE", "Import a pathway bundle, re-verify, and admit to the PathwayStore.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "SOURCE", "SOURCE")
	}
	timeout := 500
	if p.has("--z3-timeout-ms") {
		raw := p.str("--z3-timeout-ms", "")
		n, ok := pyInt(raw)
		if !ok {
			return e.usage(cmd, &usageError{fmt.Sprintf("Invalid value for '--z3-timeout-ms': '%s' is not a valid int range.", raw)})
		}
		// Z3 takes a timeout from 1 to 2**32 - 1 ms; the Python CLI
		// refuses any other at the flag, and so does this one.
		if n < 1 || n > 1<<32-1 {
			return e.usage(cmd, &usageError{fmt.Sprintf("Invalid value for '--z3-timeout-ms': %d is not in the range 1<=x<=4294967295.", n)})
		}
		timeout = int(n)
	}
	source := p.args[0]
	// The file is read and parsed before the store is opened only to
	// know, with nothing written, whether this binary can read it at all;
	// its errors are reported after the store is opened, as Python's are.
	raw, readErr := os.ReadFile(source)
	text := string(raw)
	if readErr == nil && !utf8.ValidString(text) {
		readErr = errors.New("UnicodeDecodeError: the bundle is not UTF-8")
	}
	// read_text reads with universal newlines: \r\n and a lone \r are \n.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var pw *pathways.Pathway
	var parseErr error
	if readErr == nil {
		if pathways.IsSkill(text) {
			pw, parseErr = pathways.ParseSkill(text, gateroot.PathStr(source))
		} else {
			pw, parseErr = pathways.ParseBundle(text, gateroot.PathStr(source))
		}
		if errors.Is(parseErr, pathways.ErrNotYet) {
			return e.notYet("daisugi pathways import of this skill file's frontmatter")
		}
		if errors.Is(parseErr, pathways.ErrUnreadable) {
			return e.refuse(cmd, parseErr)
		}
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	if readErr != nil {
		return e.failPy(cmd, readErr)
	}
	if parseErr != nil {
		return e.importErr(cmd, parseErr)
	}
	refusal, err := pathways.Verify(pw, timeout)
	if err != nil {
		return e.refuse(cmd, err)
	}
	if refusal != nil {
		e.errf("%s\n", refusal.Error())
		return exit(1)
	}
	if err := pathways.Storable(pw); err != nil {
		return e.importErr(cmd, err)
	}
	existed := false
	if p.flag("--overwrite") {
		if existed, err = s.ReplacePathway(pw); err != nil {
			return e.importErr(cmd, err)
		}
	} else {
		all, err := s.ReadAll(nil)
		if err != nil {
			return e.storeErr(cmd, err)
		}
		for _, x := range all {
			if x.ID() == pw.ID() {
				e.errf("[DUPLICATE_ID] pathway %s already exists; pass allow_overwrite=True to replace\n", pystr.Repr(pw.ID()))
				return exit(1)
			}
		}
		if err := s.PutPathway(pw); err != nil {
			return e.importErr(cmd, err)
		}
	}
	action := "Imported"
	if existed {
		action = "Replaced"
	}
	e.out("%s pathway %s (%s)\n", action, pw.ID(), pystr.Slice(pw.Task(), 0, 60))
	return nil
}

// importErr ends import on a parse, validation or write failure: exit 1
// with the reason, as Python's PathwayImportError or traceback does.
func (e *Env) importErr(cmd string, err error) error {
	var ie *pathways.ImportError
	if errors.As(err, &ie) {
		e.errf("%s\n", ie.Error())
		return exit(1)
	}
	return e.storeErr(cmd, err)
}

// pyInt is int(s) for a flag value: white space around it, a sign, and
// underscores between digits. Values past int64 are out of range anyway.
func pyInt(s string) (int64, bool) {
	t := strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") {
		neg = t[0] == '-'
		t = t[1:]
	}
	if t == "" || t[0] == '_' || t[len(t)-1] == '_' || strings.Contains(t, "__") {
		return 0, false
	}
	t = strings.ReplaceAll(t, "_", "")
	for _, c := range t {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	if len(t) > 18 {
		if neg {
			return -1 << 62, true
		}
		return 1 << 62, true
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// failPy is fail with the name of the exception Python raises for err,
// so an operator (and the compare) reads the same failure.
func (e *Env) failPy(cmd string, err error) error {
	name := ""
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno) && errno == syscall.EISDIR:
		name = "IsADirectoryError"
	case errors.As(err, &errno) && errno == syscall.ENOTDIR:
		name = "NotADirectoryError"
	case errors.Is(err, fs.ErrNotExist):
		name = "FileNotFoundError"
	case errors.Is(err, fs.ErrExist):
		name = "FileExistsError"
	case errors.Is(err, fs.ErrPermission):
		name = "PermissionError"
	}
	if name != "" {
		if e.tendFailed {
			e.errf("tend failed: %s: %v\n", name, err)
			return exit(1)
		}
		e.errf("daisugi %s: %s: %v\n", cmd, name, err)
		return exit(1)
	}
	return e.fail(cmd, err)
}
