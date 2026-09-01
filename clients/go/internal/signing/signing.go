// Package signing is the oracle's ed25519 signing (opendaisugi/signing.py):
// base64 keys and signatures read as Python's base64.b64decode reads them,
// sign_bytes and verify_bytes, the trusted-signer registry, and a
// contract's canonical bytes.
//
// Key format, as signing.generate_keypair writes it: the private key is
// the raw 32-byte seed in base64, the public key the raw 32 bytes in
// base64. (The oracle's docstring says PKCS#8; its code writes the raw
// seed, and this package reads what the code writes.)
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"daisugi-verify/internal/pyjson"
)

// PyError is an exception the oracle raises: its qualified type name, as
// a traceback's last line names it, and its message.
type PyError struct {
	Type string
	Msg  string
	// Context is the exception being handled when this one was raised
	// ("Type: msg"), which a traceback names first.
	Context string
}

func (e *PyError) Error() string {
	s := e.Type
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	if e.Context != "" {
		s = e.Context + "; during its handling, " + s
	}
	return s
}

// IsValueError is whether Python's `except (ValueError, TypeError)` would
// catch e: binascii.Error is a ValueError.
func (e *PyError) IsValueError() bool {
	return e.Type == "ValueError" || e.Type == "binascii.Error" || e.Type == "TypeError"
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var b64Table = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xff
	}
	for i := 0; i < len(b64Alphabet); i++ {
		t[b64Alphabet[i]] = byte(i)
	}
	return t
}()

// B64Decode is base64.b64decode(s) on a str: CPython's binascii.a2b_base64
// in its lenient mode. A character outside the alphabet is skipped; a pad
// sequence that completes a quad ends the input; what follows it is
// ignored.
func B64Decode(s string) ([]byte, *PyError) {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return nil, &PyError{Type: "ValueError", Msg: "string argument should contain only ASCII characters",
				Context: "UnicodeEncodeError: 'ascii' codec can't encode a character: ordinal not in range(128)"}
		}
	}
	var out []byte
	quad, pads := 0, 0
	var left byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					return out, nil
				}
			}
			continue
		}
		v := b64Table[c]
		if v == 0xff {
			continue
		}
		pads = 0
		switch quad {
		case 0:
			quad, left = 1, v
		case 1:
			quad = 2
			out = append(out, left<<2|v>>4)
			left = v & 0x0f
		case 2:
			quad = 3
			out = append(out, left<<4|v>>2)
			left = v & 0x03
		case 3:
			quad = 0
			out = append(out, left<<6|v)
			left = 0
		}
	}
	switch quad {
	case 0:
		return out, nil
	case 1:
		return nil, &PyError{Type: "binascii.Error", Msg: fmt.Sprintf(
			"Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4",
			len(out)/3*4+1)}
	}
	return nil, &PyError{Type: "binascii.Error", Msg: "Incorrect padding"}
}

// B64Encode is base64.b64encode(b).decode("ascii").
func B64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// privateKey is Ed25519PrivateKey.from_private_bytes(b64decode(text)).
func privateKey(privB64 string) (ed25519.PrivateKey, *PyError) {
	seed, err := B64Decode(privB64)
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, &PyError{Type: "ValueError", Msg: "An Ed25519 private key is 32 bytes long"}
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SignBytes is sign_bytes: the base64 ed25519 signature over payload.
func SignBytes(payload []byte, privB64 string) (string, *PyError) {
	k, err := privateKey(privB64)
	if err != nil {
		return "", err
	}
	return B64Encode(ed25519.Sign(k, payload)), nil
}

// PublicOf is the base64 public key of a base64 private key.
func PublicOf(privB64 string) (string, *PyError) {
	k, err := privateKey(privB64)
	if err != nil {
		return "", err
	}
	return B64Encode(k.Public().(ed25519.PublicKey)), nil
}

// VerifyBytes is verify_bytes: false on any failure (bad encoding, a key
// that is not 32 bytes, a signature that does not verify), never an error.
func VerifyBytes(payload []byte, sigB64, pubB64 string) bool {
	pub, err := B64Decode(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := B64Decode(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), payload, sig)
}

// GenerateKeypair is generate_keypair: (private_b64, public_b64), a fresh
// seed from the system's random source.
func GenerateKeypair() (priv, pub string, err error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", "", fmt.Errorf("no random key: %w", err)
	}
	k := ed25519.NewKeyFromSeed(seed)
	return B64Encode(seed), B64Encode(k.Public().(ed25519.PublicKey)), nil
}

// Registry is TrustedSignerRegistry: signer name to base64 public key.
// Entries holds the values as the JSON file holds them (a value may be
// something other than a str; the oracle keeps it as it is).
type Registry struct {
	Path    string
	Entries *pyjson.Object
}

// LoadRegistry is TrustedSignerRegistry.load: an absent file is an empty
// registry; a file that is not a JSON object is an error, as the oracle
// raises.
func LoadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Registry{Path: path, Entries: pyjson.NewObject()}, nil
	}
	if err != nil {
		return nil, err
	}
	v, derr := pyjson.LoadsPy(string(raw), 900)
	if derr != nil {
		return nil, &PyError{Type: "json.decoder.JSONDecodeError", Msg: derr.Error()}
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Type: "ValueError", Msg: fmt.Sprintf("%s: expected dict, got %s", path, pyTypeName(v))}
	}
	return &Registry{Path: path, Entries: o}, nil
}

func pyTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case pyjson.Int, int:
		return "int"
	case float64, pyjson.Float:
		return "float"
	case nil:
		return "NoneType"
	}
	return "dict"
}

// Save is TrustedSignerRegistry.save: json.dumps(sort_keys=True, indent=2).
func (r *Registry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o777); err != nil {
		return err
	}
	keys := append([]string{}, r.Entries.Keys()...)
	sort.Strings(keys)
	sorted := pyjson.NewObject()
	for _, k := range keys {
		sorted.Set(k, r.Entries.Value(k))
	}
	return os.WriteFile(r.Path, []byte(pyjson.DumpsIndent(sorted, 2, true)), 0o666)
}

// Add sets name to the key.
func (r *Registry) Add(name, pubB64 string) { r.Entries.Set(name, pubB64) }

// Remove deletes name and reports whether it was there.
func (r *Registry) Remove(name string) bool {
	if _, ok := r.Entries.Get(name); !ok {
		return false
	}
	r.Entries.Delete(name)
	return true
}

// Get is the value named, or nil when absent.
func (r *Registry) Get(name string) (any, bool) { return r.Entries.Get(name) }

// Names is the sorted names.
func (r *Registry) Names() []string {
	out := append([]string{}, r.Entries.Keys()...)
	sort.Strings(out)
	return out
}

// VerifyWith is verify_bytes with a registry value as the key. A value
// that is not a str raises TypeError in b64decode, which verify_bytes
// catches: false.
func VerifyWith(payload []byte, sigB64 string, pub any) bool {
	s, ok := pub.(string)
	if !ok {
		return false
	}
	return VerifyBytes(payload, sigB64, s)
}
