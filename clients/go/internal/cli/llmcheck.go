package cli

import (
	"os"
	"path/filepath"

	"daisugi-verify/internal/llmcheck"
	"daisugi-verify/internal/verify"
)

// llmCheck is llm_check.run_llm_check for every verify a command runs:
// the backend resolve_backend() names, through the one model client. The
// client, and so the neutral working directory `claude -p` runs in, is
// made once per process, as the oracle makes it.
func (e *Env) llmCheck(rule, payload string) verify.LLMVerdict {
	if e.checkClient == nil {
		e.checkClient = e.llmClient()
	}
	res, unported := llmcheck.Run(llmcheck.Env{Getenv: e.lookup, Vars: e.env,
		Backend: e.checkClient.Backend(), Claude: e.checkClient}, rule, payload)
	return verify.LLMVerdict{Satisfied: res.Satisfied, Reason: res.Reason, Errored: res.Errored, Unported: unported}
}

// gettempdir is tempfile.gettempdir(): the first of $TMPDIR, $TEMP, $TMP,
// /tmp, /var/tmp and /usr/tmp that is a directory a file can be made in,
// made absolute.
func (e *Env) gettempdir() string {
	var dirs []string
	for _, k := range []string{"TMPDIR", "TEMP", "TMP"} {
		if v := e.env[k]; v != "" {
			dirs = append(dirs, v)
		}
	}
	dirs = append(dirs, "/tmp", "/var/tmp", "/usr/tmp")
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		f, err := os.CreateTemp(abs, "probe")
		if err != nil {
			continue
		}
		name := f.Name()
		f.Close()
		os.Remove(name)
		return abs
	}
	return "/tmp"
}
