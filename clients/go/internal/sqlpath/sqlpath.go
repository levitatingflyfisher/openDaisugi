// Package sqlpath names a database file to the SQLite driver as Python's
// sqlite3.connect(path) names it: a plain file name, never a URI and
// never options. The driver reads a '?' in a plain DSN as the start of
// its options and a leading "file:" as a URI, so the name goes to it as a
// file: URI of the absolute path, every special character escaped.
package sqlpath

import (
	"net/url"
	"path/filepath"
)

// DSN is the driver's name for the file at path, with query (such as
// "mode=ro") added when it is not empty.
func DSN(path, query string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := (&url.URL{Scheme: "file", Path: abs}).String()
	if query != "" {
		u += "?" + query
	}
	return u, nil
}
