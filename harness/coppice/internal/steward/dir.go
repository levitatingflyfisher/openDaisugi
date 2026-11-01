package steward

import (
	"os"
	"path/filepath"

	"github.com/opendaisugi/coppice/internal/datahome"
)

// DirName is the steward's log directory name on disk. It keeps the name it
// had before the rename. ADR-0023 moves this store to
// `<data home>/pando/<store>/` with `daisugi pando migrate`; one move, not
// two. The backup areas carry the same name, so an old backup restores.
const DirName = "foreman" // names-ok: the on-disk name until ADR-0023 moves the store

// DefaultDir is where the steward keeps its chat when no --data-dir is
// given: $OPENDAISUGI_HOME/DirName when that is set; else
// $XDG_DATA_HOME/opendaisugi/DirName when that is set, unless an old
// ~/.opendaisugi/coppice holds data, which keeps ~/.opendaisugi/DirName;
// else ~/.opendaisugi/DirName (internal/datahome).
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil && os.Getenv("OPENDAISUGI_HOME") == "" {
		return "", err
	}
	return defaultDir(os.Getenv, home, datahome.Has), nil
}

// LogDir is the steward's log dir for a coppice data dir. The default
// data dir (the data home's coppice) uses DefaultDir, the steward dir
// beside it; any other data dir X uses X/DirName. The coppice server
// writes the log there and coppice steward reads it there.
func LogDir(coppiceDataDir string) string {
	if filepath.Clean(coppiceDataDir) == filepath.Join(datahome.Default(), "coppice") {
		if d, err := DefaultDir(); err == nil {
			return d
		}
	}
	return filepath.Join(coppiceDataDir, DirName)
}

func defaultDir(getenv func(string) string, home string, has func(string) bool) string {
	return filepath.Join(datahome.Dir(getenv, home, has), DirName)
}

// StoreRoot is the data home that holds pando/<store> for a coppice data
// dir. The default data dir uses the data home itself, so coppice and
// `daisugi pando` find the same store. Any other data dir X keeps its stores
// in X/pando, so a run with its own --data-dir never touches the real
// store.
func StoreRoot(coppiceDataDir string) string {
	if filepath.Clean(coppiceDataDir) == filepath.Join(datahome.Default(), "coppice") {
		return datahome.Default()
	}
	return coppiceDataDir
}
