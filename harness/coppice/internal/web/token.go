package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TokenBytes is the entropy behind one web token. Encoded with
// base64.RawURLEncoding it is 43 characters, and every one of them is a legal
// RFC 7230 token character. That matters: a browser cannot set an
// Authorization header on a WebSocket, so the same string also rides a
// Sec-WebSocket-Protocol offer, where an "=" padding character is illegal.
const TokenBytes = 32

// ErrNoToken says nobody has minted a token on this box yet.
var ErrNoToken = errors.New("no web token")

// TokenStore is the operator's bearer token, held in a file mode 0600
// inside a directory mode 0700, and the named tokens beside it. The
// operator's token has no name. Each named token carries the name it was
// minted for.
type TokenStore struct{ Path string }

// NameRule is the refusal for a name that breaks the name rule.
const NameRule = "names are one word, up to 32 characters"

// NameMax is the longest name, in characters.
const NameMax = 32

// CheckName refuses a name that is empty, longer than NameMax, or holds a
// character other than an ASCII letter, a digit, a dash or an underscore.
func CheckName(name string) error {
	if name == "" || len(name) > NameMax {
		return errors.New(NameRule)
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return errors.New(NameRule)
		}
	}
	return nil
}

// namedToken is one row of the names file.
type namedToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// NamesPath is the file that holds the named tokens, beside the
// operator's token.
func (s TokenStore) NamesPath() string { return s.Path + "-names.json" }

// loadNamed reads the named tokens. A missing file is no named tokens. A
// file it cannot read or parse is an error, and every caller refuses the
// named tokens on it.
func (s TokenStore) loadNamed() ([]namedToken, error) {
	raw, err := os.ReadFile(s.NamesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var body struct {
		Tokens []namedToken `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", s.NamesPath(), err)
	}
	return body.Tokens, nil
}

// saveNamed writes the named tokens through a temporary file and a rename.
func (s TokenStore) saveNamed(rows []namedToken) error {
	body, err := json.Marshal(map[string]any{"tokens": rows})
	if err != nil {
		return err
	}
	return writePrivate(s.NamesPath(), ".web-names-*", body)
}

// MintFor writes a fresh token for name and retires any token name had.
func (s TokenStore) MintFor(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	unlock, err := s.lockNames()
	if err != nil {
		return "", err
	}
	defer unlock()
	rows, err := s.loadNamed()
	if err != nil {
		return "", err
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	kept := []namedToken{{Name: name, Token: tok}}
	for _, r := range rows {
		if r.Name != name {
			kept = append(kept, r)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	if err := s.saveNamed(kept); err != nil {
		return "", err
	}
	return tok, nil
}

// Lookup returns the token minted for name, or ErrNoToken when name holds
// none.
func (s TokenStore) Lookup(name string) (string, error) {
	rows, err := s.loadNamed()
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Name == name && r.Token != "" {
			return r.Token, nil
		}
	}
	return "", ErrNoToken
}

// Revoke retires the token minted for name.
func (s TokenStore) Revoke(name string) error {
	unlock, err := s.lockNames()
	if err != nil {
		return err
	}
	defer unlock()
	rows, err := s.loadNamed()
	if err != nil {
		return err
	}
	kept := rows[:0:0]
	for _, r := range rows {
		if r.Name != name {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(rows) {
		return fmt.Errorf("no token has the name %s. Run: coppice web token list", name)
	}
	return s.saveNamed(kept)
}

// Names lists the names that hold a token, in order. It never returns a
// token.
func (s TokenStore) Names() ([]string, error) {
	rows, err := s.loadNamed()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, nil
}

// Who checks candidate against every token in constant time and returns
// the name it carries. The operator's token carries no name. ok is false
// for a blank candidate or one that matches no token. A names file it
// cannot read refuses every named token and leaves the operator's token.
func (s TokenStore) Who(candidate string) (name string, ok bool) {
	if candidate == "" {
		return "", false
	}
	c := []byte(candidate)
	if stored, err := s.Load(); err == nil && subtle.ConstantTimeCompare(c, []byte(stored)) == 1 {
		ok = true
	}
	rows, err := s.loadNamed()
	if err != nil {
		return "", ok
	}
	for _, r := range rows {
		if r.Token == "" || CheckName(r.Name) != nil {
			continue
		}
		if subtle.ConstantTimeCompare(c, []byte(r.Token)) == 1 && !ok {
			name, ok = r.Name, true
		}
	}
	return name, ok
}

// Load returns the stored token. A missing file, an unreadable file, or a
// blank file all return ErrNoToken, never an empty string a caller might
// compare against.
func (s TokenStore) Load() (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", ErrNoToken
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

// Mint writes a fresh token and replaces any token already there. It writes
// to a temporary file in the same directory first, then renames it into
// place, so a reader never sees a half-written token.
func (s TokenStore) Mint() (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := writePrivate(s.Path, ".web-token-*", []byte(tok)); err != nil {
		return "", err
	}
	return tok, nil
}

// newToken is TokenBytes of fresh randomness in unpadded base64url.
func newToken() (string, error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// writePrivate writes body to path mode 0600 in a directory mode 0700. It
// writes a temporary file in the same directory first, then renames it
// into place, so a reader never sees half a file.
func writePrivate(path, pattern string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Verify reports whether candidate is any token this store holds, the
// operator's or a named one. It fails closed. A missing or blank token
// file and a blank candidate never authorize.
func (s TokenStore) Verify(candidate string) bool {
	_, ok := s.Who(candidate)
	return ok
}
