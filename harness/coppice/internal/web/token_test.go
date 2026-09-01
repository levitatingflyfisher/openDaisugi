package web

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func newStore(t *testing.T) TokenStore {
	t.Helper()
	return TokenStore{Path: filepath.Join(t.TempDir(), "coppice", "web-token")}
}

// The failure this names: a box with no token file must not be an open box.
func TestVerifyRefusesEverythingWhenNoTokenHasBeenMinted(t *testing.T) {
	s := newStore(t)
	if s.Verify("anything") {
		t.Fatal("an absent token file authorized a request")
	}
	if s.Verify("") {
		t.Fatal("an empty candidate authorized a request")
	}
}

// The failure this names: an empty token file must not match an empty header.
func TestVerifyRefusesWhenTheTokenFileIsEmpty(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Verify("") || s.Verify("   ") {
		t.Fatal("an empty token file authorized a request")
	}
	if _, err := s.Load(); err != ErrNoToken {
		t.Fatalf("Load on an empty file returned %v, want ErrNoToken", err)
	}
}

// The failure this names: every other test builds its path under a fresh
// t.TempDir(), so the directory never exists before Mint runs, and the
// explicit Chmod that tightens a looser pre-existing directory never runs
// either. A box where something else created the web directory at a wider
// mode must still end up at 0700 once Mint has run.
func TestMintTightensAPreExistingDirectoryTo0700(t *testing.T) {
	s := newStore(t)
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mint(); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("a pre-existing directory at 0777 is %o after Mint, want 700", di.Mode().Perm())
	}
}

func TestMintWritesAPrivateFileInAPrivateDirectory(t *testing.T) {
	s := newStore(t)
	if _, err := s.Mint(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode is %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(s.Path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("token directory mode is %o, want 700", di.Mode().Perm())
	}
}

// A token also rides a Sec-WebSocket-Protocol header, where "=" is illegal.
// Raw base64url keeps every character legal there.
func TestMintedTokenIsBase64URLWithNoPadding(t *testing.T) {
	s := newStore(t)
	tok, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(tok) {
		t.Fatalf("token %q is not 43 characters of unpadded base64url", tok)
	}
}

func TestMintRotatesAndTheOldTokenStopsWorking(t *testing.T) {
	s := newStore(t)
	first, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Mint returned the same token twice")
	}
	if s.Verify(first) {
		t.Fatal("the rotated-out token still authorizes")
	}
	if !s.Verify(second) {
		t.Fatal("the current token does not authorize")
	}
}

// The CLI resolves the data directory once and passes it in. This package
// never reads the home directory on its own, so the default path is a
// function of that data directory, not a lookup.
func TestTokenPathIsWebTokenUnderTheDataDir(t *testing.T) {
	got := TokenPath("/srv/coppice-data")
	want := filepath.Join("/srv/coppice-data", "web", "token")
	if got != want {
		t.Fatalf("TokenPath returned %q, want %q", got, want)
	}
}
