package verify

import "errors"

// This file holds what tree.edge_ok needs from the subsumption port: each
// answer is a verdict, never a solver model, so every client prints the
// same reasons.

// GlobUnsupported is subsumption._glob_unsupported: a glob shape the
// proof cannot read.
func GlobUnsupported(glob string) bool { return globUnsupported(glob) }

// RecognizedOpaque is _invariant_types.RECOGNIZED_OPAQUE_TYPES.
func RecognizedOpaque(t string) bool { return recognizedOpaqueTypes[t] }

// Fit is the answer of PatternFits.
type Fit int

const (
	// Fits: every value the pattern admits, the outer patterns admit too.
	Fits Fit = iota
	// DoesNotFit: Z3 found a value they do not admit.
	DoesNotFit
	// Unfinished: the proof did not finish, or no Z3 is linked.
	Unfinished
)

// PatternFits is tree._pattern_fits.
func PatternFits(pattern string, outer []string, timeoutMs int) Fit {
	if len(outer) == 0 {
		return DoesNotFit
	}
	z3c, err := sharedZ3()
	if err != nil {
		return Unfinished
	}
	smt2 := "(declare-const v String)\n(assert " + globToZ3("v", pattern) + ")\n(assert (not " +
		smtOr(mapGlob("v", outer)) + "))\n"
	res, err := z3c.CheckSat(smt2, timeoutMs)
	switch {
	case err != nil:
		return Unfinished
	case res == "unsat":
		return Fits
	case res == "sat":
		return DoesNotFit
	}
	return Unfinished
}

// SubsumesStrict is envelope_subsumes(outer, inner, strict=True) for the
// edge rule: whether it holds, and whether the proof did not finish (a
// timeout, or no Z3). A proof that does not finish never holds.
func SubsumesStrict(outer, inner Envelope, timeoutMs int) (holds, unfinished bool) {
	z3c, err := sharedZ3()
	if err != nil {
		return false, true
	}
	ok, err := EnvelopeSubsumes(z3c, outer, inner, timeoutMs, true)
	var timeout *SubsumptionTimeout
	if errors.As(err, &timeout) || err != nil {
		return false, true
	}
	return ok, false
}
