package cli

// version is overridable at link time:
// go build -ldflags "-X github.com/opendaisugi/coppice/internal/cli.version=X.Y.Z"
var version = "0.1.0"

// Version returns the build's version string.
func Version() string { return version }
