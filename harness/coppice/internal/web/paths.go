package web

import "path/filepath"

// Every default path in this package is a function of the data directory
// the caller resolved, never a lookup of its own. Nothing here reads the
// home directory or an environment variable.

// TokenPath is where the bearer token lives under a data directory: a
// "web" directory holding a file named "token".
func TokenPath(dataDir string) string {
	return filepath.Join(dataDir, "web", "token")
}

// TLSDir is where coppice web cert tailscale writes the pair it asks
// tailscale for: a "tls" directory under the data directory.
func TLSDir(dataDir string) string {
	return filepath.Join(dataDir, "web", "tls")
}

// TailscalePaths are the certificate and key coppice web cert tailscale
// writes under TLSDir. coppice web serve reads the same pair back with
// --tls tailscale when no --cert or --key is given, so both commands agree
// on one pair of names instead of each inventing its own.
func TailscalePaths(dataDir string) (string, string) {
	dir := TLSDir(dataDir)
	return filepath.Join(dir, "tailscale.crt"), filepath.Join(dir, "tailscale.key")
}

// LocalCADir is where coppice web cert init writes the CA: a "ca" directory
// under the data directory.
func LocalCADir(dataDir string) string {
	return filepath.Join(dataDir, "web", "ca")
}

// ConfigPath is where the phone server's settings are saved: a "web.json"
// file under the data directory. Enabled inside that file is what makes
// the coppice server start the phone server on its own.
func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "web", "web.json")
}

// GateRoot is the openDaisugi gate directory beside the coppice data
// directory. The gate and the coppice data directory are both children of
// the same openDaisugi home by default, so the gate root is the data
// directory's own parent, plus "gate", cleaned of the ".." this produces.
func GateRoot(dataDir string) string {
	return filepath.Join(dataDir, "..", "gate")
}
