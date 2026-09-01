// Package deeds is the oracle's deed ledger (opendaisugi/deeds.py): a
// run's reversible file writes undone from the journal alone, with no
// model, no executor and no re-run.
package deeds

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/signing"
	"daisugi-verify/internal/tracejournal"
)

func none() any { return nil }

// HandleModel is models.ReversalHandle.
var HandleModel = &pmodel.Model{Name: "ReversalHandle", Fields: []pmodel.Field{
	{Name: "kind", Schema: pmodel.Literal{Choices: []string{"file_write"}}, Required: true},
	{Name: "path", Schema: pmodel.Str{}, Required: true},
	{Name: "prior_existed", Schema: pmodel.Bool{}, Required: true},
	{Name: "prior_content", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: none},
	{Name: "created_dirs", Schema: pmodel.List{Elem: pmodel.Str{}}, Default: func() any { return []any{} }},
	{Name: "note", Schema: pmodel.Str{}, Default: func() any { return "" }},
}}

// Handle is a validated ReversalHandle.
type Handle struct {
	Kind         string
	Path         string
	PriorExisted bool
	PriorContent *string
	CreatedDirs  []string
}

// ParseHandle validates a handle dump as ReversalHandle.model_validate.
func ParseHandle(v any) (*Handle, *pmodel.ValidationError) {
	out, err := pmodel.Validate("ReversalHandle", HandleModel, v, pmodel.Python)
	if err != nil {
		return nil, err
	}
	o := out.(*pyjson.Object)
	h := &Handle{Kind: o.Value("kind").(string), Path: o.Value("path").(string), PriorExisted: o.Value("prior_existed").(bool)}
	if s, ok := o.Value("prior_content").(string); ok {
		h.PriorContent = &s
	}
	for _, d := range o.Value("created_dirs").([]any) {
		h.CreatedDirs = append(h.CreatedDirs, d.(string))
	}
	return h, nil
}

// atomicWrite is _atomic_write: the parent made, the text written to a
// temporary name beside the target, then renamed over it.
func atomicWrite(path, content string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("no random name: %w", err)
	}
	tmp := filepath.Join(parent, ".daisugi-undo-"+hex.EncodeToString(b)+"-"+filepath.Base(path))
	if err := os.WriteFile(tmp, []byte(content), 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Apply is apply_reversal: the prior content written back when the target
// existed; otherwise the file removed (absent is fine) and each directory
// the write made removed, deepest first, when it is empty.
func Apply(h *Handle) error {
	if h.Kind != "file_write" {
		return &signing.PyError{Type: "ValueError", Msg: fmt.Sprintf("cannot reverse deed of kind '%s'", h.Kind)}
	}
	if h.PriorExisted {
		c := ""
		if h.PriorContent != nil {
			c = *h.PriorContent
		}
		return atomicWrite(h.Path, c)
	}
	// os.unlink: an absent file is fine; anything else (a directory, a
	// parent that is a file) raises, as in the oracle.
	if err := syscall.Unlink(h.Path); err != nil && err != syscall.ENOENT {
		return &os.PathError{Op: "unlink", Path: h.Path, Err: err}
	}
	for _, d := range h.CreatedDirs {
		_ = syscall.Rmdir(d)
	}
	return nil
}

// Report is RollbackReport.
type Report struct {
	Undone  []string
	Skipped []*pyjson.Object
}

// Dump is dataclasses.asdict(report).
func (r *Report) Dump() *pyjson.Object {
	u := make([]any, len(r.Undone))
	for i, s := range r.Undone {
		u[i] = s
	}
	sk := make([]any, len(r.Skipped))
	for i, s := range r.Skipped {
		sk[i] = s
	}
	return pyjson.NewObject().Set("undone", u).Set("skipped", sk)
}

// Rollback is rollback_run: the run's reversible deeds undone newest
// first; irreversible deeds reported as skipped; read-only deeds left.
func Rollback(j *tracejournal.Journal, runID string) (*Report, error) {
	rows, err := j.Receipts(runID)
	if err != nil {
		return nil, err
	}
	rep := &Report{Undone: []string{}, Skipped: []*pyjson.Object{}}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Reversibility == "reversible" && r.Reversal != nil {
			h, verr := ParseHandle(r.Reversal)
			if verr != nil {
				return nil, verr
			}
			if err := Apply(h); err != nil {
				return nil, err
			}
			rep.Undone = append(rep.Undone, h.Path)
		} else if r.Reversibility == "irreversible" {
			rep.Skipped = append(rep.Skipped, pyjson.NewObject().Set("step_id", r.StepID).
				Set("effect_class", r.EffectClass).Set("reason", "irreversible"))
		}
	}
	return rep, nil
}

// PathState is PathState: what was at a path before the run first wrote it.
type PathState struct {
	Path       string
	PreExisted bool
	PreContent *string
}

// Touched is touched_files: each file the run wrote with a handle, in the
// order first written, with the state before its first write.
func Touched(j *tracejournal.Journal, runID string) ([]PathState, error) {
	rows, err := j.Receipts(runID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []PathState
	for _, r := range rows {
		if r.EffectClass != "file_write" || r.Reversal == nil {
			continue
		}
		h, verr := ParseHandle(r.Reversal)
		if verr != nil {
			return nil, verr
		}
		if seen[h.Path] {
			continue
		}
		seen[h.Path] = true
		out = append(out, PathState{Path: h.Path, PreExisted: h.PriorExisted, PreContent: h.PriorContent})
	}
	return out, nil
}
