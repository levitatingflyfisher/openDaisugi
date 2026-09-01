package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
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

// TokenStore is the one bearer token, held in a file mode 0600 inside a
// directory mode 0700.
type TokenStore struct{ Path string }

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
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)
	tmp, err := os.CreateTemp(dir, ".web-token-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(tok); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return "", err
	}
	return tok, nil
}

// Verify compares candidate against the stored token in constant time and
// fails closed. A missing token file, a blank token file, and a blank
// candidate all refuse, none of them ever authorizes.
func (s TokenStore) Verify(candidate string) bool {
	if candidate == "" {
		return false
	}
	stored, err := s.Load()
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(stored)) == 1
}
