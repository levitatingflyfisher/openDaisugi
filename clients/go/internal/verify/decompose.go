package verify

import (
	"sort"

	"daisugi-verify/internal/shell"
)

// Decomposition is shell_decompose.Decomposition.
type Decomposition = shell.Decomposition

// DecomposeCommand is shell_decompose.decompose_command, on
// tree-sitter-bash, the oracle's own grammar (see internal/shell).
func DecomposeCommand(command string) Decomposition { return shell.DecomposeCommand(command) }

// SortedCopy returns a sorted copy of ss.
func SortedCopy(ss []string) []string {
	out := make([]string, len(ss))
	copy(out, ss)
	sort.Strings(out)
	return out
}
