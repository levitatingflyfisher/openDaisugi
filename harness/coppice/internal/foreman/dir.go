package foreman

import (
	"os"
	"path/filepath"

	"github.com/opendaisugi/coppice/internal/datahome"
)

// DefaultDir is where the foreman keeps its chat when no --data-dir is
// given: $OPENDAISUGI_HOME/foreman when that is set; else
// $XDG_DATA_HOME/opendaisugi/foreman when that is set and ~/.opendaisugi
// does not exist; else ~/.opendaisugi/foreman (internal/datahome).
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil && os.Getenv("OPENDAISUGI_HOME") == "" {
		return "", err
	}
	return defaultDir(os.Getenv, home, datahome.Exists), nil
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

func defaultDir(getenv func(string) string, home string, exists func(string) bool) string {
	return filepath.Join(datahome.Dir(getenv, home, exists), "foreman")
}
