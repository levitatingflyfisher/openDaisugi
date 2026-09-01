// Package shipped holds the plugins the coppice binary carries. Each
// directory is one plugin with a manifest.json. The embed line names each
// directory, so a file whose name starts with _ or . inside one, a Python
// cache among them, never ships.
package shipped

import (
	"embed"
	"io/fs"
)

//go:embed _lib tree minimap kanban colony shift-log herdr-grid inbox merge-on-green close-quiet turn-budget notify-ntfy notify-lockscreen notify-voice
var files embed.FS

// Files is the shipped plugins, one directory per plugin id.
func Files() fs.FS { return files }
