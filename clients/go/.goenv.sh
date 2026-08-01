export GOFLAGS=-p=2
# Resolve the module cache/paths relative to this script's own directory, so
# the file carries no machine-specific absolute path (public repo).
_GOENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
export GOPATH="$_GOENV_DIR/.gopath"
export GOCACHE="$_GOENV_DIR/.gocache"
export GOMODCACHE="$_GOENV_DIR/.gomodcache"
