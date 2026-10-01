package server

import "github.com/opendaisugi/coppice/internal/foreman"

// snapshotGrowBytes is how many bytes of lines may sit in <newer> before the
// snapshot is frozen again. Each freeze is one cache write; between freezes
// the input carries the added lines, so a small bound keeps it small.
const snapshotGrowBytes = 16 << 10

// splitView says which lines of the view are the frozen snapshot and which
// were added since. The first turn freezes a snapshot. After that it is
// frozen again when its lines are no longer the first lines of the view (a
// batch merge that touches it does this), or when the lines added since pass
// snapshotGrowBytes. The caller holds the chat lock.
func (fc *foremanChat) splitView(lines []foreman.ViewLine) (snapshot, added []byte) {
	keep := fc.snapSet && len(fc.snap) <= len(lines)
	for i := 0; keep && i < len(fc.snap); i++ {
		keep = fc.snap[i] == lines[i]
	}
	if keep {
		grown := 0
		for _, l := range lines[len(fc.snap):] {
			grown += len(l.Text)
		}
		keep = grown <= snapshotGrowBytes
	}
	if !keep {
		fc.snap, fc.snapSet = append([]foreman.ViewLine(nil), lines...), true
	}
	for i, l := range lines {
		if i < len(fc.snap) {
			snapshot = append(snapshot, l.Text...)
		} else {
			added = append(added, l.Text...)
		}
	}
	return snapshot, added
}
