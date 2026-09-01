package cli

import (
	"os"
	"unicode/utf8"

	"daisugi-verify/internal/batch"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/signing"
)

// This file is `daisugi batch prove` (opendaisugi/batch.py): a declared
// batch proved before any item runs.

const batchHelp = `Usage: daisugi batch [OPTIONS] COMMAND [ARGS]...

  Within-instance batch compilation: prove a declared batch before it runs.

Commands:
  prove  Statically prove a declared batch before any iteration.
`

func (e *Env) batchCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", batchHelp)
		return nil
	}
	if args[0] == "prove" {
		return e.batchProve(args[1:])
	}
	e.errf("Usage: daisugi batch [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi batch --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

// readText is path.read_text(): the file's text, which must be UTF-8.
func readText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", &signing.PyError{Type: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode the file " + path}
	}
	return string(raw), nil
}

func (e *Env) batchProve(args []string) error {
	const cmd = "batch prove"
	opts := []opt{
		{names: []string{"--envelope", "-e"}, value: true, metavar: "PATH", help: "Envelope JSON to prove the footprint against."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "DECLARATION", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " DECLARATION", "Statically prove a declared batch before any iteration.", opts)
	}
	if len(p.args) == 0 {
		return e.usageArgs(cmd, "DECLARATION", &usageError{"Missing argument 'declaration'."})
	}
	if !p.has("--envelope") {
		return e.usageArgs(cmd, "DECLARATION", &usageError{"Missing option '--envelope' / '-e'."})
	}
	text, err := readText(gateroot.PathStr(p.args[0]))
	if err != nil {
		return e.raise(cmd, err)
	}
	decl, verr := batch.ValidateJSON(text)
	if verr != nil {
		return e.raise(cmd, verr)
	}
	etext, err := readText(gateroot.PathStr(p.str("--envelope", "")))
	if err != nil {
		return e.raise(cmd, err)
	}
	env, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, etext)
	if verr != nil {
		return e.raise(cmd, verr)
	}
	cls := batch.Classify(decl)
	if !cls.Batchable {
		e.echoErr("NOT BATCHABLE — program contains non-batchable step kind(s): %s\n", cls.NonBatchableKinds())
		return exit(1)
	}
	perms := env.(*pyjson.Object).Value("permissions").(*pyjson.Object)
	var fw []string
	for _, g := range perms.Value("file_write").([]any) {
		fw = append(fw, g.(string))
	}
	proof, err := batch.Prove(decl, fw)
	if err != nil {
		return e.raise(cmd, err)
	}
	for _, w := range proof.Writes {
		e.echo("  write: %s\n", w)
	}
	irr := batch.Irreversible(proof.Writes)
	if proof.OK && len(irr) == 0 {
		e.echo("PROVABLE — %d write(s), all inside the envelope and the declared footprint F; one proof covers all N.\n",
			len(proof.Writes))
		return nil
	}
	if !proof.OK {
		e.echoErr("UNPROVABLE — %s\n", proof.Reason)
	}
	if len(irr) > 0 {
		e.echoErr("IRREVERSIBLE TARGETS (cannot enter a batch): %s\n", strListRepr(irr))
	}
	return exit(1)
}

func strListRepr(xs []string) string {
	if len(xs) == 0 {
		return "[]"
	}
	return pystr.ReprList(xs)
}
