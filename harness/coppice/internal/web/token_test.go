package web

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
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

// A token minted for a name carries that name, and the operator's own
// token carries none.
func TestAMintedTokenCarriesItsName(t *testing.T) {
	s := newStore(t)
	main, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	alice, err := s.MintFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := s.Who(alice); !ok || name != "alice" {
		t.Fatalf("Who(alice's token) = %q, %v, want alice, true", name, ok)
	}
	if name, ok := s.Who(main); !ok || name != "" {
		t.Fatalf("Who(the main token) = %q, %v, want no name, true", name, ok)
	}
	if !s.Verify(alice) || !s.Verify(main) {
		t.Fatal("a good token did not verify")
	}
	if _, ok := s.Who("nope"); ok {
		t.Fatal("a wrong token was accepted")
	}
	if _, ok := s.Who(""); ok {
		t.Fatal("an empty token was accepted")
	}
	names, err := s.Names()
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("Names() = %v, %v, want [alice]", names, err)
	}
}

// A named token works with no main token minted, and a second mint for the
// same name retires the first.
func TestMintForTheSameNameRetiresTheOldToken(t *testing.T) {
	s := newStore(t)
	first, err := s.MintFor("bob")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.MintFor("bob")
	if err != nil {
		t.Fatal(err)
	}
	if s.Verify(first) {
		t.Fatal("the retired token for bob still works")
	}
	if name, ok := s.Who(second); !ok || name != "bob" {
		t.Fatalf("Who(bob's new token) = %q, %v", name, ok)
	}
	fi, err := os.Stat(s.NamesPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("names file mode is %o, want 600", fi.Mode().Perm())
	}
}

func TestRevokeEndsANamedToken(t *testing.T) {
	s := newStore(t)
	tok, err := s.MintFor("carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("carol"); err != nil {
		t.Fatal(err)
	}
	if s.Verify(tok) {
		t.Fatal("a revoked token still works")
	}
	if err := s.Revoke("carol"); err == nil {
		t.Fatal("revoking a name with no token did not say so")
	}
}

func TestANameMustBeOneShortWord(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", "has space", "a.b", "tab\tname", "ali:ce", "ålice",
		"abcdefghijklmnopqrstuvwxyz0123456"} {
		if _, err := s.MintFor(bad); err == nil || err.Error() != NameRule {
			t.Fatalf("MintFor(%q) = %v, want %q", bad, err, NameRule)
		}
	}
	for _, good := range []string{"a", "alice", "Bob_2", "night-shift", "abcdefghijklmnopqrstuvwxyz012345"} {
		if err := CheckName(good); err != nil {
			t.Fatalf("CheckName(%q) = %v", good, err)
		}
	}
}

// A names file nobody can read refuses every named token and leaves the
// operator's own token working. The names file here held alice's token
// before it became unreadable, so the refusal is of a token that was good.
func TestABadNamesFileKeepsTheMainTokenAndRefusesTheNamedOnes(t *testing.T) {
	for _, spoil := range []struct {
		name string
		do   func(path string) error
	}{
		{"not json", func(path string) error { return os.WriteFile(path, []byte("{not json"), 0o600) }},
		{"a directory", func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o700)
		}},
	} {
		t.Run(spoil.name, func(t *testing.T) {
			s := newStore(t)
			main, err := s.Mint()
			if err != nil {
				t.Fatal(err)
			}
			alice, err := s.MintFor("alice")
			if err != nil {
				t.Fatal(err)
			}
			if !s.Verify(alice) {
				t.Fatal("alice's token did not work before the file was spoiled")
			}
			if err := spoil.do(s.NamesPath()); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.Who(alice); ok {
				t.Fatal("a named token worked with an unreadable names file")
			}
			if name, ok := s.Who(main); !ok || name != "" {
				t.Fatalf("the main token = %q, %v, want it to work with no name", name, ok)
			}
			if _, err := s.MintFor("bob"); err == nil {
				t.Fatal("a mint over a bad names file did not refuse")
			}
			if _, err := s.Names(); err == nil {
				t.Fatal("Names over a bad names file did not refuse")
			}
		})
	}
}

// Two mints at once keep both names. Each one reads the file, changes it
// and writes it back, so without a lock one of the two is lost.
func TestMintsAtOnceKeepEveryName(t *testing.T) {
	s := newStore(t)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.MintFor(fmt.Sprintf("p%02d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	names, err := s.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != n {
		t.Fatalf("%d names kept, want %d: %v", len(names), n, names)
	}
}
