package foreman

import (
	"os"
	"path/filepath"

	"github.com/opendaisugi/coppice/internal/datahome"
)

// DefaultDir is where the foreman keeps its chat when no --data-dir is
// given: $OPENDAISUGI_HOME/foreman when that is set; else
// $XDG_DATA_HOME/opendaisugi/foreman when that is set, unless an old
// ~/.opendaisugi/coppice holds data, which keeps ~/.opendaisugi/foreman;
// else ~/.opendaisugi/foreman (internal/datahome).
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil && os.Getenv("OPENDAISUGI_HOME") == "" {
		return "", err
	}
	return defaultDir(os.Getenv, home, datahome.Has), nil
}

// LogDir is the foreman's log dir for a coppice data dir. The default
// data dir (the data home's coppice) uses DefaultDir, the foreman dir
// beside it; any other data dir X uses X/foreman. The coppice server
// writes the log there and coppice foreman reads it there.
func LogDir(coppiceDataDir string) string {
	if filepath.Clean(coppiceDataDir) == filepath.Join(datahome.Default(), "coppice") {
		if d, err := DefaultDir(); err == nil {
			return d
		}
	}
	return filepath.Join(coppiceDataDir, "foreman")
}

func defaultDir(getenv func(string) string, home string, has func(string) bool) string {
	return filepath.Join(datahome.Dir(getenv, home, has), "foreman")
}
