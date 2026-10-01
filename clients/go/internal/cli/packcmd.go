package cli

import (
	"path/filepath"
	"strings"

	"daisugi-verify/internal/catalog"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pack"
)

var packHelp = `Usage: daisugi pack [OPTIONS] COMMAND [ARGS]...

  The ML packs: a pinned Python with locked packages, for the jobs that
  stay in Python.

Options:
  --help  Show this message and exit.

Commands:
  bundle   Fetch a pack's Python and wheels into OUT, for pack install --offline.
  install  Install a pack: the pinned Python, a virtual environment, the locked wheels.
  list     List the packs and which are installed.
  remove   Delete a pack's directory, and nothing else.
  run      Run one job in a pack's worker.
  status   Check the installed packs against their pins.
`

var loraHelp = `Usage: daisugi lora [OPTIONS] COMMAND [ARGS]...

  LoRA training: the trainer runs in the train pack.

Options:
  --help  Show this message and exit.

Commands:
  train   Train a LoRA adapter (python -m opendaisugi.lora.train's arguments).
  export  Not yet in this binary.
`

var loraTrainHelp = `Usage: daisugi lora train --jsonl JSONL --output OUTPUT [TRAINER OPTIONS]

  Train a LoRA adapter in the train pack (daisugi pack install train).
  Every option is the trainer's (python -m opendaisugi.lora.train). The
  base model is --base-model, else the choice daisugi models use recorded
  in --data-dir, else the default for this hardware.
`

// packCtx is the pack context: the catalog OPENDAISUGI_PACK_CATALOG names
// (read here only), else the built-in one.
func (e *Env) packCtx(dataDir string) (*pack.Ctx, error) {
	cat, err := pack.Load(e.env[pack.CatalogEnv])
	if err != nil {
		return nil, err
	}
	return &pack.Ctx{
		DataDir: dataDir,
		Cat:     cat,
		Out:     func(s string) { e.out("%s\n", s) },
		Err:     func(s string) { e.errf("%s\n", s) },
		Env:     e.Environ,
		SystemDir: func() string {
			if v := e.env[pack.SystemEnv]; v != "" {
				return v
			}
			return pack.SystemPacks
		}(),
	}, nil
}

func (e *Env) packDataDir(p *parsed) string {
	return gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
}

// packCmd is the `pack` group (cli.pack_app).
func (e *Env) packCmd(args []string) error {
	if len(args) == 0 {
		e.out("%s", packHelp)
		return exit(2)
	}
	if args[0] == "--help" {
		e.out("%s", packHelp)
		return nil
	}
	type spec struct {
		args    string
		max     int
		summary string
		opts    []opt
	}
	offline := opt{names: []string{"--offline"}, value: true, metavar: "DIR", help: "Install from a bundle (a directory or a .tar)."}
	force := opt{names: []string{"--force"}, help: "Install again over an installed pack."}
	specs := map[string]spec{
		"list":    {"", 0, "List the packs and which are installed.", []opt{dataDirOpt}},
		"status":  {" [NAME]", 1, "Check the installed packs against their pins.", []opt{dataDirOpt}},
		"install": {" NAME", 1, "Install a pack: the pinned Python, a virtual environment, the locked wheels.", []opt{offline, force, dataDirOpt}},
		"remove":  {" NAME", 1, "Delete a pack's directory, and nothing else.", []opt{dataDirOpt}},
		"bundle":  {" NAME OUT", 2, "Fetch a pack's Python and wheels into OUT, for pack install --offline.", []opt{dataDirOpt}},
	}
	sub := args[0]
	if sub == "run" {
		return e.packRun(args[1:])
	}
	s, ok := specs[sub]
	if !ok {
		e.errf("Usage: daisugi pack [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi pack --help' for help.\n\nError: No such command '%s'.\n", sub)
		return exit(2)
	}
	cmd := "pack " + sub
	p, err := parseArgs(args[1:], s.opts, s.max)
	if err != nil {
		return e.usageArgs(cmd, strings.TrimSpace(s.args), err)
	}
	if p.help {
		return e.cmdHelp(cmd, s.args, s.summary, s.opts)
	}
	need := strings.Count(s.args, " ") - strings.Count(s.args, "[")
	if len(p.args) < need {
		missing := strings.Fields(s.args)[len(p.args)]
		return e.usageArgs(cmd, strings.TrimSpace(s.args), &usageError{"Missing argument '" + missing + "'."})
	}
	c, err := e.packCtx(e.packDataDir(p))
	if err != nil {
		return e.fail(cmd, err)
	}
	var code int
	switch sub {
	case "list":
		code = pack.List(c)
	case "status":
		name := ""
		if len(p.args) > 0 {
			name = p.args[0]
		}
		code = pack.Status(c, name)
	case "install":
		var off *string
		if p.has("--offline") {
			v := p.str("--offline", "")
			off = &v
		}
		code = pack.Install(c, p.args[0], off, p.flag("--force"))
	case "remove":
		code = pack.Remove(c, p.args[0])
	case "bundle":
		code = pack.Bundle(c, p.args[0], p.args[1])
	}
	if code != 0 {
		return exit(code)
	}
	return nil
}

// packRun is `pack run [--data-dir P] NAME JOB [ARGS]...`: options only
// before NAME; every word after JOB goes to the job.
func (e *Env) packRun(args []string) error {
	const cmd = "pack run"
	dataDir := filepath.Join(e.home, ".opendaisugi")
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		a := args[i]
		switch {
		case a == "--help":
			return e.cmdHelp(cmd, " NAME JOB [ARGS]...", "Run one job in a pack's worker. Options go before NAME; every word after JOB goes to the job.", []opt{dataDirOpt})
		case a == "--data-dir":
			if i+1 >= len(args) {
				return e.usageArgs(cmd, "NAME JOB [ARGS]...", &usageError{"Option '--data-dir' requires an argument."})
			}
			dataDir = args[i+1]
			i += 2
			continue
		case strings.HasPrefix(a, "--data-dir="):
			dataDir = strings.TrimPrefix(a, "--data-dir=")
		case a == "--":
			i++
			goto positional
		default:
			return e.usageArgs(cmd, "NAME JOB [ARGS]...", &usageError{"No such option: " + a})
		}
		i++
	}
positional:
	rest := args[i:]
	if len(rest) < 1 {
		return e.usageArgs(cmd, "NAME JOB [ARGS]...", &usageError{"Missing argument 'NAME'."})
	}
	if len(rest) < 2 {
		return e.usageArgs(cmd, "NAME JOB [ARGS]...", &usageError{"Missing argument 'JOB'."})
	}
	c, err := e.packCtx(gateroot.PathStr(dataDir))
	if err != nil {
		return e.fail(cmd, err)
	}
	if code := pack.Run(c, rest[0], rest[1], rest[2:]); code != 0 {
		return exit(code)
	}
	return nil
}

// loraCmd is the `lora` group: train runs in the train pack; export stays
// in Python.
func (e *Env) loraCmd(args []string) error {
	if len(args) == 0 {
		e.out("%s", loraHelp)
		return exit(2)
	}
	switch args[0] {
	case "--help":
		e.out("%s", loraHelp)
		return nil
	case "train":
		return e.loraTrain(args[1:])
	case "export":
		return e.notYet("daisugi lora export")
	}
	e.errf("Usage: daisugi lora [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi lora --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

// flagValue is worker.flag_value.
func flagValue(args []string, flag string) (string, bool) {
	got, ok := "", false
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			got, ok = args[i+1], true
		} else if strings.HasPrefix(a, flag+"=") {
			got, ok = a[len(flag)+1:], true
		}
	}
	return got, ok
}

// loraTrain is cli.lora_train_cmd on its pack path (MC-R-6 picks the base
// model when --base-model is not given).
func (e *Env) loraTrain(args []string) error {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			e.out("%s", loraTrainHelp)
			return nil
		}
	}
	dataDir := filepath.Join(e.home, ".opendaisugi")
	if v, ok := flagValue(args, "--data-dir"); ok {
		dataDir = v
	}
	dataDir = gateroot.PathStr(dataDir)
	if _, ok := flagValue(args, "--base-model"); !ok {
		base := inUse(dataDir)
		if base == "" {
			cat, err := catalog.Load()
			if err != nil {
				return e.fail("lora train", err)
			}
			hw := e.modelsHardware()
			base = cat.Default(hw.RAMGB, hw.VRAMGB)
		}
		args = append([]string{"--base-model", base}, args...)
	}
	c, err := e.packCtx(dataDir)
	if err != nil {
		return e.fail("lora train", err)
	}
	if code := pack.LoraTrain(c, args); code != 0 {
		return exit(code)
	}
	return nil
}
