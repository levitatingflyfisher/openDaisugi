package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/capture"
	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

func (e *Env) hook(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("Usage: daisugi hook [OPTIONS] COMMAND [ARGS]...\n\n  Capture hooks. This binary carries auto-tend.\n\n" +
			"Commands:\n  auto-tend  Close the captures to traces to distillation loop in one call.\n")
		return nil
	}
	if args[0] == "auto-tend" {
		return e.autoTend(args[1:])
	}
	return e.notYet("daisugi hook " + args[0])
}

// conversion is one session this run converts, checked before anything
// is written.
type conversion struct {
	session capture.Session
	task    string
	env     *pyjson.Object
	plan    *pyjson.Object
	result  *pyjson.Object
	// skip is why captures_to_trace raised a ValueError the command
	// prints and skips; the session is not converted.
	skip string
}

// prepareConversion is captures_to_trace up to the journal write: the
// records read, the envelope inferred, the plan built and verified. A
// capture whose trace would carry a violation is refused: the binary does
// not word every violation's detail yet.
func prepareConversion(s capture.Session, decompose bool) (*conversion, error) {
	recs, err := capture.Records(s.Path)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	task := "captured session " + s.ID
	env, err := capture.InferEnvelope(recs, task, decompose)
	if err != nil {
		return nil, err
	}
	plan, err := capture.Plan(recs, task)
	var invalid *capture.InvalidPlan
	if errors.As(err, &invalid) {
		return &conversion{session: s, skip: invalid.Text}, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := verify.ParsePlan([]byte(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", capture.ErrUnreadable, err)
	}
	en, err := verify.ParseEnvelope([]byte(pathways.DumpJSON(env)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", capture.ErrUnreadable, err)
	}
	t0 := time.Now()
	res := verify.Verify(p, en, verify.VerifyOptions{Z3TimeoutMs: 500})
	ms := float64(time.Since(t0).Nanoseconds()) / 1e6
	if !res.OK {
		return nil, fmt.Errorf("%w: session %s would be journaled with a violation (%s), whose detail this binary does not write yet",
			capture.ErrUnreadable, s.ID, res.Violations[0].Message)
	}
	warnings := []any{}
	for _, w := range res.Timeouts {
		warnings = append(warnings, w)
	}
	result := pyjson.NewObject().Set("ok", true).Set("violations", []any{}).Set("warnings", warnings).
		Set("envelope_id", env.Value("id")).Set("plan_id", plan.Value("id")).Set("duration_ms", ms).
		Set("client", "python").Set("fallback", nil).Set("client_verdict", nil)
	// The body is written after other sessions are: one this binary
	// cannot write must refuse now, before the first write.
	if _, err := tracejournal.TraceBody(task, env, plan, result, "2000-01-01-00000000", "2000-01-01T00:00:00Z"); err != nil {
		return nil, err
	}
	return &conversion{session: s, task: task, env: env, plan: plan, result: result}, nil
}

func (e *Env) autoTend(args []string) error {
	const cmd = "hook auto-tend"
	opts := []opt{
		{names: []string{"--captures-root"}, value: true, metavar: "PATH"},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH"},
		{names: []string{"--min-interval"}, value: true, metavar: "INTEGER", help: "Skip if last auto-tend was newer than this many seconds."},
		{names: []string{"--force"}, help: "Ignore the min-interval gate."},
		{names: []string{"--skip-distill"}, help: "Convert captures-to-traces but don't run tend afterwards."},
		{names: []string{"--allow-shell-decomposition"}, neg: "--no-allow-shell-decomposition",
			help: "Let the envelope admit compound shell (ADR-0010)."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Close the captures->traces->distillation loop in one cron-friendly call.", opts)
	}
	minInterval, err := clickInt(p, "--min-interval", 3600)
	if err != nil {
		return e.usage(cmd, err)
	}
	root := p.str("--captures-root", filepath.Join(e.home, ".opendaisugi", "captures"))
	dataDir := p.str("--data-dir", filepath.Join(e.home, ".opendaisugi"))
	force := p.flag("--force")
	cfg, err := config.Load(filepath.Join(dataDir, "config.yaml"))
	if errors.Is(err, config.ErrInvalid) {
		e.errf("daisugi %s: pydantic_core._pydantic_core.ValidationError: the config file does not validate\n", cmd)
		return exit(1)
	}
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("the config file is not one this binary reads: %w", err))
	}
	if !force && (cfg.AutoTend == nil || !*cfg.AutoTend) {
		e.out("skipped: background distillation is off. Run `daisugi install` to opt in, or pass --force to tend once anyway.\n")
		return nil
	}
	stamp := filepath.Join(dataDir, ".hook-auto-tend-last-run")
	now := nowSeconds()
	last, err := readStamp(stamp)
	if err != nil {
		return e.failPy(cmd, err)
	}
	if !force && now-last < float64(minInterval) {
		e.out("skipped: last run %ds ago (< --min-interval=%d); use --force to override\n", int64(now-last), minInterval)
		return nil
	}
	decompose := cfg.ShellAllowDecomposition
	if p.flagSet("--allow-shell-decomposition") {
		decompose = p.flag("--allow-shell-decomposition")
	}
	// Everything this run would convert is read and checked first, so a
	// capture this binary cannot convert changes nothing.
	sessions, err := capture.ListSessions(root)
	if err != nil {
		return e.autoTendErr(cmd, err)
	}
	// Read only: the journal is made or migrated after every refusal.
	var jr *tracejournal.Journal
	if _, err := os.Stat(filepath.Join(dataDir, "journal", "index.db")); err == nil {
		if jr, err = tracejournal.OpenReadOnly(dataDir); err != nil {
			return e.failPy(cmd, err)
		}
		defer jr.Close()
	}
	var todo []*conversion
	var skipped []string
	for _, s := range sessions {
		if jr != nil {
			done, err := jr.IsConverted(s.ID)
			if err != nil {
				return e.failPy(cmd, err)
			}
			if done {
				continue
			}
		}
		c, err := prepareConversion(s, decompose)
		if err != nil {
			return e.autoTendErr(cmd, err)
		}
		if c == nil {
			skipped = append(skipped, s.ID)
		}
		todo = append(todo, c)
	}
	nconv := 0
	for _, c := range todo {
		if c != nil && c.skip == "" {
			nconv++
		}
	}
	if nconv > 0 && !p.flag("--skip-distill") {
		// The tend that follows the conversions must not refuse after
		// they are written: its checks run now, with the conversions
		// counted in.
		if _, err := e.tendCheck(dataDir, "anthropic/claude-sonnet-4-20250514", 3, 30, false, nconv, nowSeconds()); err != nil {
			// Daisugi() raises only after the conversions, in Python; that
			// one is reported when tend runs.
			var raised *raisedError
			if !errors.As(err, &raised) {
				return err
			}
		}
	}
	// The trace ids are drawn before the first write, so a failed draw
	// changes nothing.
	ids := map[string]string{}
	for _, c := range todo {
		if c != nil && c.skip == "" {
			if ids[c.session.ID], err = randHex8(); err != nil {
				return e.failPy(cmd, err)
			}
		}
	}
	if jr != nil {
		jr.Close()
	}
	j, err := tracejournal.Open(dataDir)
	if err != nil {
		return e.failPy(cmd, err)
	}
	defer j.Close()
	var converted []string
	for _, s := range sessions {
		c := findConversion(todo, s.ID)
		if c != nil && c.skip != "" {
			e.errf("  skipped %s: %s\n", s.ID, c.skip)
			continue
		}
		if c == nil {
			for _, sk := range skipped {
				if sk == s.ID {
					e.errf("  skipped %s: no records in %s\n", s.ID, filepath.Join(root, s.ID+".jsonl"))
				}
			}
			continue
		}
		created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		traceID := created[:10] + "-" + ids[s.ID]
		if err := j.Log(c.task, c.env, c.plan, c.result, traceID, created); err != nil {
			return e.autoTendErr(cmd, err)
		}
		if err := j.MarkConverted(s.ID, traceID, nowSeconds()); err != nil {
			return e.failPy(cmd, err)
		}
		converted = append(converted, traceID)
		e.out("  converted %s → %s\n", s.ID, traceID)
	}
	e.out("converted %d sessions\n", len(converted))
	if len(converted) > 0 && !p.flag("--skip-distill") {
		rep, err := e.runTendQuiet(dataDir)
		if err != nil {
			return err
		}
		if rep != nil {
			e.out("tend: created=%d updated=%d skipped=%d\n", rep.Created, rep.Updated, rep.Skipped)
		}
	}
	if err := os.MkdirAll(dataDir, 0o777); err != nil {
		return e.failPy(cmd, err)
	}
	if err := os.WriteFile(stamp, []byte(pyjson.FloatRepr(now)), 0o666); err != nil {
		return e.failPy(cmd, err)
	}
	return nil
}

// runTendQuiet is Daisugi(data_dir=...).tend() as auto-tend runs it: the
// defaults, and a failure reported as "tend failed" rather than raised.
func (e *Env) runTendQuiet(dataDir string) (*tendReport, error) {
	e.tendFailed = true
	defer func() { e.tendFailed = false }()
	rep, err := e.runTend(dataDir, "anthropic/claude-sonnet-4-20250514", 3, 30, false)
	if err != nil {
		if e.raised {
			return nil, err
		}
		var x *exitError
		if errors.As(err, &x) && x.code == 1 {
			// runTend printed its reason as the command's error; auto-tend
			// catches the exception and goes on.
			return nil, nil
		}
		return nil, err
	}
	return &rep, nil
}

func findConversion(todo []*conversion, id string) *conversion {
	for _, c := range todo {
		if c != nil && c.session.ID == id {
			return c
		}
	}
	return nil
}

// randRead is crypto/rand.Read; a test replaces it.
var randRead = rand.Read

// randHex8 is secrets.token_hex(4). A failed read is an error, never a
// fixed id.
func randHex8() (string, error) {
	b := make([]byte, 4)
	if _, err := randRead(b); err != nil {
		return "", fmt.Errorf("no random trace id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (e *Env) autoTendErr(cmd string, err error) error {
	if errors.Is(err, capture.ErrUnreadable) || errors.Is(err, tracejournal.ErrUnreadable) {
		return e.refuse(cmd, err)
	}
	return e.failPy(cmd, err)
}

// tendFailure is how auto-tend reports a tend that raised: "tend failed:
// <exception class name>: <message>".
func tendFailure(msg string) string {
	head, rest, found := strings.Cut(msg, ": ")
	if found && !strings.Contains(head, " ") {
		if i := strings.LastIndex(head, "."); i >= 0 {
			head = head[i+1:]
		}
		return "tend failed: " + head + ": " + rest
	}
	return "tend failed: " + msg
}

var _ = pystr.Repr
