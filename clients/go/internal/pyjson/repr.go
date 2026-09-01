package pyjson

import "daisugi-verify/internal/pystr"

// Repr is Python's repr(str) (see pystr.Repr).
func Repr(s string) string { return pystr.Repr(s) }

// ReprList is Python's repr(list[str]).
func ReprList(ss []string) string { return pystr.ReprList(ss) }
