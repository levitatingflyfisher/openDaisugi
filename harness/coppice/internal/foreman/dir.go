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

func defaultDir(getenv func(string) string, home string, exists func(string) bool) string {
	return filepath.Join(datahome.Dir(getenv, home, exists), "foreman")
}
